// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package p2p

import (
	"container/heap"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/netutil"
)

const (
	// This is the amount of time spent waiting in between
	// redialing a certain node.
	dialHistoryExpiration = 30 * time.Second

	// Discovery lookups are throttled and can only run
	// once every few seconds.
	lookupInterval = 4 * time.Second

	// If no peers are found for this amount of time, the initial bootnodes are
	// attempted to be connected.
	fallbackInterval = 20 * time.Second

	// Endpoint resolution is throttled with bounded backoff.
	initialResolveDelay = 60 * time.Second
	maxResolveDelay     = time.Hour
)

// NodeDialer is used to connect to nodes in the network, typically by using
// an underlying net.Dialer but also using net.Pipe in tests
type NodeDialer interface {
	Dial(*discover.Node) (net.Conn, error)
}

// TCPDialer implements the NodeDialer interface by using a net.Dialer to
// create TCP connections to nodes in the network
type TCPDialer struct {
	*net.Dialer
}

// Dial creates a TCP connection to the node
func (t TCPDialer) Dial(dest *discover.Node) (net.Conn, error) {
	addr := &net.TCPAddr{IP: dest.IP, Port: int(dest.TCP)}
	return t.Dialer.Dial("tcp", addr.String())
}

// dialstate schedules dials and discovery lookups.
// it get's a chance to compute new tasks on every iteration
// of the main loop in Server.run.
type dialstate struct {
	maxDynDials int 		//最大的动态节点链接数量
	ntab        discoverTable		//discoverTable 用来做节点查询的
	netrestrict *netutil.Netlist

	lookupRunning bool
	dialing       map[discover.NodeID]connFlag		//正在链接的节点
	lookupBuf     []*discover.Node // current discovery lookup results //当前的discovery查询结果
	randomNodes   []*discover.Node // filled from Table  //从discoverTable随机查询的节点
	static        map[discover.NodeID]*dialTask		//静态的节点。
	hist          *dialHistory

	start     time.Time        // time when the dialer was first used
	bootnodes []*discover.Node // default dials when there are no peers		//这个是内置的节点。 如果没有找到其他节点。那么使用链接这些节点。
}

type discoverTable interface {
	Self() *discover.Node
	Close()
	Resolve(target discover.NodeID) *discover.Node
	Lookup(target discover.NodeID) []*discover.Node
	ReadRandomNodes([]*discover.Node) int
}

// the dial history remembers recent dials.
type dialHistory []pastDial

// pastDial is an entry in the dial history.
type pastDial struct {
	id  discover.NodeID
	exp time.Time
}

type task interface {
	Do(*Server)
}

// A dialTask is generated for each node that is dialed. Its
// fields cannot be accessed while the task is running.
type dialTask struct {
	flags        connFlag
	dest         *discover.Node
	lastResolved time.Time
	resolveDelay time.Duration
}

// discoverTask runs discovery table operations.
// Only one discoverTask is active at any time.
// discoverTask.Do performs a random lookup.
type discoverTask struct {
	results []*discover.Node
}

// A waitExpireTask is generated if there are no other tasks
// to keep the loop in Server.run ticking.
type waitExpireTask struct {
	time.Duration
}

func newDialState(static []*discover.Node, bootnodes []*discover.Node, ntab discoverTable, maxdyn int, netrestrict *netutil.Netlist) *dialstate {
	s := &dialstate{
		maxDynDials: maxdyn,
		ntab:        ntab,
		netrestrict: netrestrict,
		static:      make(map[discover.NodeID]*dialTask),
		dialing:     make(map[discover.NodeID]connFlag),
		bootnodes:   make([]*discover.Node, len(bootnodes)),
		randomNodes: make([]*discover.Node, maxdyn/2),
		hist:        new(dialHistory),
	}
	copy(s.bootnodes, bootnodes)
	for _, n := range static {
		s.addStatic(n)
	}
	return s
}

func (s *dialstate) addStatic(n *discover.Node) {
	// This overwites the task instead of updating an existing
	// entry, giving users the opportunity to force a resolve operation.
	s.static[n.ID] = &dialTask{flags: staticDialedConn, dest: n}
}

func (s *dialstate) removeStatic(n *discover.Node) {
	// This removes a task so future attempts to connect will not be made.
	delete(s.static, n.ID)
	// This removes a previous dial timestamp so that application
	// can force a server to reconnect with chosen peer immediately.
	s.hist.remove(n.ID)
}

func (s *dialstate) newTasks(nRunning int, peers map[discover.NodeID]*Peer, now time.Time) []task {
	if s.start.IsZero() {
		s.start = now
	}

	var newtasks []task
	addDial := func(flag connFlag, n *discover.Node) bool {
		if err := s.checkDial(n, peers); err != nil {
			//log.Info("Skipping dial candidate", "id", n.ID, "addr", &net.TCPAddr{IP: n.IP, Port: int(n.TCP)}, "err", err)
			log.Trace("Skipping dial candidate", "id", n.ID, "addr", &net.TCPAddr{IP: n.IP, Port: int(n.TCP)}, "err", err)
			return false
		}
		s.dialing[n.ID] = flag
		newtasks = append(newtasks, &dialTask{flags: flag, dest: n})
		return true
	}

	// Compute number of dynamic dials necessary at this point.
	needDynDials := s.maxDynDials
	//首先判断已经建立的连接的类型。如果是动态类型。那么需要建立动态链接数量减少。
	//log.Info("len(peers) = ",len(peers))
	for _, p := range peers {
		if p.rw.is(dynDialedConn) {
			needDynDials--
		}
	}
	//然后再判断正在建立的链接。如果是动态类型。那么需要建立动态链接数量减少。
	for _, flag := range s.dialing {
		if flag&dynDialedConn != 0 {
			needDynDials--
		}
	}

	// Expire the dial history on every invocation.
	s.hist.expire(now)

	// Create dials for static nodes if they are not connected.
	//查看所有的静态类型。如果可以那么也创建链接。
	for id, t := range s.static {
		err := s.checkDial(t.dest, peers)
		switch err {
		case errNotWhitelisted, errSelf:
			log.Warn("Removing static dial candidate", "id", t.dest.ID, "addr", &net.TCPAddr{IP: t.dest.IP, Port: int(t.dest.TCP)}, "err", err)
			delete(s.static, t.dest.ID)
		case nil:
			s.dialing[id] = t.flags
			newtasks = append(newtasks, t)
		}
	}
	// If we don't have any peers whatsoever, try to dial a random bootnode. This
	// scenario is useful for the testnet (and private networks) where the discovery
	// table might be full of mostly bad peers, making it hard to find good ones.
	//如果当前还没有任何链接。 而且20秒(fallbackInterval)内没有创建任何链接。 那么就使用bootnode创建链接。
	if len(peers) == 0 && len(s.bootnodes) > 0 && needDynDials > 0 && now.Sub(s.start) > fallbackInterval {
		bootnode := s.bootnodes[0]
		s.bootnodes = append(s.bootnodes[:0], s.bootnodes[1:]...)
		s.bootnodes = append(s.bootnodes, bootnode)

		if addDial(dynDialedConn, bootnode) {
			needDynDials--
		}
	}
	// Use random nodes from the table for half of the necessary
	// dynamic dials.
	//否则使用1/2的随机节点创建链接。
	randomCandidates := needDynDials / 2
	if randomCandidates > 0 {
		n := s.ntab.ReadRandomNodes(s.randomNodes)
		for i := 0; i < randomCandidates && i < n; i++ {
			if addDial(dynDialedConn, s.randomNodes[i]) {
				needDynDials--
			}
		}
	}
	// Create dynamic dials from random lookup results, removing tried
	// items from the result buffer.
	i := 0
	for ; i < len(s.lookupBuf) && needDynDials > 0; i++ {
		if addDial(dynDialedConn, s.lookupBuf[i]) {
			needDynDials--
		}
	}
	s.lookupBuf = s.lookupBuf[:copy(s.lookupBuf, s.lookupBuf[i:])]
	// Launch a discovery lookup if more candidates are needed.
	// 如果就算这样也不能创建足够动态链接。 那么创建一个discoverTask用来再网络上查找其他的节点。放入lookupBuf
	if len(s.lookupBuf) < needDynDials && !s.lookupRunning {
		s.lookupRunning = true
		newtasks = append(newtasks, &discoverTask{})
	}

	// Launch a timer to wait for the next node to expire if all
	// candidates have been tried and no task is currently active.
	// This should prevent cases where the dialer logic is not ticked
	// because there are no pending events.
	// 如果当前没有任何任务需要做，那么创建一个睡眠的任务返回。
	if nRunning == 0 && len(newtasks) == 0 && s.hist.Len() > 0 {
		t := &waitExpireTask{s.hist.min().exp.Sub(now)}
		newtasks = append(newtasks, t)
	}
	return newtasks
}

var (
	errSelf             = errors.New("is self")
	errAlreadyDialing   = errors.New("already dialing")
	errAlreadyConnected = errors.New("already connected")
	errRecentlyDialed   = errors.New("recently dialed")
	errNotWhitelisted   = errors.New("not contained in netrestrict whitelist")
)

func (s *dialstate) checkDial(n *discover.Node, peers map[discover.NodeID]*Peer) error {
	_, dialing := s.dialing[n.ID]
	switch {
	case dialing:		//正在创建
		return errAlreadyDialing
	case peers[n.ID] != nil:		//已经链接了
		return errAlreadyConnected
	case s.ntab != nil && n.ID == s.ntab.Self().ID:		//建立的对象不是自己
		return errSelf
	case s.netrestrict != nil && !s.netrestrict.Contains(n.IP):		//网络限制。 对方的IP地址不在白名单里面。
		return errNotWhitelisted
	case s.hist.contains(n.ID):		// 这个ID曾经链接过。
		return errRecentlyDialed
	}
	return nil
}

/**
taskDone方法。 这个方法再task完成之后会被调用。 查看task的类型。如果是链接任务，那么增加到hist里面。
并从正在链接的队列删除。 如果是查询任务。 把查询的记过放在lookupBuf里面。
 */
func (s *dialstate) taskDone(t task, now time.Time) {
	switch t := t.(type) {
	case *dialTask:
		s.hist.add(t.dest.ID, now.Add(dialHistoryExpiration))
		delete(s.dialing, t.dest.ID)
	case *discoverTask:
		s.lookupRunning = false
		s.lookupBuf = append(s.lookupBuf, t.results...)
	}
}

/**
不同的task有不同的Do方法。 dailTask主要负责建立链接。 如果t.dest是没有ip地址的。
那么尝试通过resolve查询ip地址。 然后调用dial方法创建链接。 对于静态的节点。如果第一次失败，
那么会尝试再次resolve静态节点。然后再尝试dial（因为静态节点的ip是配置的。
如果静态节点的ip地址变动。那么我们尝试resolve静态节点的新地址，然后调用链接。）
 */
func (t *dialTask) Do(srv *Server) {
	if t.dest.Incomplete() {
		if !t.resolve(srv) {
			return
		}
	}
	err := t.dial(srv, t.dest)
	if err != nil {
		log.Trace("Dial error", "task", t, "err", err)
		// Try resolving the ID of static nodes if dialing failed.
		if _, ok := err.(*dialError); ok && t.flags&staticDialedConn != 0 {
			if t.resolve(srv) {
				t.dial(srv, t.dest)
			}
		}
	}
}

// resolve attempts to find the current endpoint for the destination
// using discovery.
//
// Resolve operations are throttled with backoff to avoid flooding the
// discovery network with useless queries for nodes that don't exist.
// The backoff delay resets when the node is found.
//resolve方法。这个方法主要调用了discover网络的Resolve方法。如果失败，那么超时再试
func (t *dialTask) resolve(srv *Server) bool {
	if srv.ntab == nil {
		log.Debug("Can't resolve node", "id", t.dest.ID, "err", "discovery is disabled")
		return false
	}
	if t.resolveDelay == 0 {
		t.resolveDelay = initialResolveDelay
	}
	if time.Since(t.lastResolved) < t.resolveDelay {
		return false
	}
	resolved := srv.ntab.Resolve(t.dest.ID)
	t.lastResolved = time.Now()
	if resolved == nil {
		t.resolveDelay *= 2
		if t.resolveDelay > maxResolveDelay {
			t.resolveDelay = maxResolveDelay
		}
		log.Debug("Resolving node failed", "id", t.dest.ID, "newdelay", t.resolveDelay)
		return false
	}
	// The node was found.
	t.resolveDelay = initialResolveDelay
	t.dest = resolved
	log.Debug("Resolved node", "id", t.dest.ID, "addr", &net.TCPAddr{IP: t.dest.IP, Port: int(t.dest.TCP)})
	return true
}

type dialError struct {
	error
}

// dial performs the actual connection attempt.
//dial方法,这个方法进行了实际的网络连接操作。 主要通过srv.SetupConn方法来完成， 后续再分析Server.go的时候再分析这个方法
func (t *dialTask) dial(srv *Server, dest *discover.Node) error {
	fd, err := srv.Dialer.Dial(dest)
	if err != nil {
		log.Info("dial error ip = ", dest.IP.String(), ",err", err.Error())

		return &dialError{err}
	}
	mfd := newMeteredConn(fd, false)
	return srv.SetupConn(mfd, t.flags, dest)
}

func (t *dialTask) String() string {
	return fmt.Sprintf("%v %x %v:%d", t.flags, t.dest.ID[:8], t.dest.IP, t.dest.TCP)
}

func (t *discoverTask) Do(srv *Server) {
	// newTasks generates a lookup task whenever dynamic dials are
	// necessary. Lookups need to take some time, otherwise the
	// event loop spins too fast.
	next := srv.lastLookup.Add(lookupInterval)
	if now := time.Now(); now.Before(next) {
		time.Sleep(next.Sub(now))
	}
	srv.lastLookup = time.Now()
	var target discover.NodeID
	rand.Read(target[:])
	t.results = srv.ntab.Lookup(target)
}

func (t *discoverTask) String() string {
	s := "discovery lookup"
	if len(t.results) > 0 {
		s += fmt.Sprintf(" (%d results)", len(t.results))
	}
	return s
}

func (t waitExpireTask) Do(*Server) {
	time.Sleep(t.Duration)
}
func (t waitExpireTask) String() string {
	return fmt.Sprintf("wait for dial hist expire (%v)", t.Duration)
}

// Use only these methods to access or modify dialHistory.
func (h dialHistory) min() pastDial {
	return h[0]
}
func (h *dialHistory) add(id discover.NodeID, exp time.Time) {
	heap.Push(h, pastDial{id, exp})

}
func (h *dialHistory) remove(id discover.NodeID) bool {
	for i, v := range *h {
		if v.id == id {
			heap.Remove(h, i)
			return true
		}
	}
	return false
}
func (h dialHistory) contains(id discover.NodeID) bool {
	for _, v := range h {
		if v.id == id {
			return true
		}
	}
	return false
}
func (h *dialHistory) expire(now time.Time) {
	for h.Len() > 0 && h.min().exp.Before(now) {
		heap.Pop(h)
	}
}

// heap.Interface boilerplate
func (h dialHistory) Len() int           { return len(h) }
func (h dialHistory) Less(i, j int) bool { return h[i].exp.Before(h[j].exp) }
func (h dialHistory) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dialHistory) Push(x interface{}) {
	*h = append(*h, x.(pastDial))
}
func (h *dialHistory) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
