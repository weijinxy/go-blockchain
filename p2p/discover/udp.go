package discover

import (
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

const (
	pingPacket = iota + 1
	pongPacket
	findnodePacket
	replynodePacket
)

var (
	expirationTime = 30 * time.Second
)

var (
	errPacketTimeout = errors.New("timeout")
	errPacketHandle  = errors.New("handle packet error")
	errListTimeout   = errors.New("queue time out")
)

type udp struct {
	conn     conn
	pending  chan *pending
	gotreply chan gotreply
	self     endpoint
	tab      *Table
	Id       NodeID
	exit     chan struct{}
}

//待处理
type pending struct {
	typ      byte
	deadline int64
	callback func(v interface{}) bool
	errch    chan error
}

// 返回处理
type gotreply struct {
	typ     byte
	data    interface{}
	matched chan bool
}

type endpoint struct {
	IP       net.IP
	UDP, TCP int
}

type conn interface {
	ReadFromUDP(b []byte) (int, *net.UDPAddr, error)
	WriteToUDP(b []byte, to *net.UDPAddr) (int, error)
	Close() error
	LocalAddr() net.Addr
}

func ListenUDP(cfg Config) *udp {
	laddr, err := net.ResolveUDPAddr("udp", cfg.Laddr)
	if err != nil {
		log.Println("resolve net udpaddr error", err)
		return nil
	}

	c, err := net.ListenUDP("udp", laddr)
	if err != nil {
		log.Println("fail to listen udp,", ",err", err)
		return nil
	}

	return newUDP(c, cfg)
}

func newUDP(c conn, cfg Config) *udp {
	t := udp{
		conn:     c,
		pending:  make(chan *pending, 10),
		gotreply: make(chan gotreply, 10),
		Id:       StringID(cfg.Id),
		exit:     make(chan struct{}),
	}

	tab := newTable(&t, cfg)
	t.tab = tab

	go t.taskLoop()
	go t.readLoop()

	return &t
}

func (t *udp) taskLoop() {
	var plist = list.New()
	listClear := func() {
		for pl := plist.Front(); pl != nil; pl = pl.Next() {
			if v, ok := pl.Value.(*pending); ok {
				if v.deadline < time.Now().Unix() {
					v.errch <- errListTimeout
					plist.Remove(pl)
				}
			} else {
				plist.Remove(pl)
			}
		}
	}

	for {
		//
		listClear()
		select {
		case p := <-t.pending:
			p.deadline = time.Now().Add(1 * time.Minute).Unix() //有效时间1分
			plist.PushBack(p)
		case r := <-t.gotreply:
			match := false
			fmt.Println("reply:", plist.Len(), " type:", r.typ)
			for pl := plist.Front(); pl != nil; pl = pl.Next() {
				v := pl.Value.(*pending)
				fmt.Println(">", r.typ, v.typ)
				if r.typ == v.typ {
					// 处理回调函数
					if v.callback(r.data) {
						v.errch <- nil
						plist.Remove(pl)
					}
				}
				match = true
			}
			r.matched <- match
		}
	}
}

func (t *udp) readLoop() {
	buf := make([]byte, 1028)
	for {
		nbytes, from, err := t.conn.ReadFromUDP(buf)
		if err != nil && err != io.EOF {
			log.Println("read from udp error", err)
			t.exit <- struct{}{}
			return
		}

		if err == io.EOF {
			log.Println("recv eof")
			continue
		}
		log.Println("recv handle <=", from)
		if err = t.handleRequest(buf[:nbytes], from); err != nil {
			log.Println(err)
			// 处理失败
		}
	}
}

func (t *udp) handleRequest(buf []byte, to *net.UDPAddr) error {
	pack, fromID, err := decodePacket(buf)
	if err != nil {
		return err
	}
	//log.Println("fromID", fromID)
	err = pack.handle(t, fromID, to)
	return err
}

func (t *udp) sendMessage(typ byte, to *net.UDPAddr, pack packet) {
	data := encodePacket(t.Id, typ, pack)
	_, err := t.conn.WriteToUDP(data, to)
	if err != nil {
		log.Println("write to udp error:", err)
	}
}

// 添加待处理的事件
func (t *udp) addPending(typ byte, call func(v interface{}) bool) <-chan error {
	ch := make(chan error, 1)
	select {
	case t.pending <- &pending{typ: typ, callback: call, errch: ch}:
		// todo
	case <-t.exit:
		ch <- errors.New("udp pending exit")
	}
	return ch
}

// 处理返回的结果
func (t *udp) handleReply(typ byte, pack packet) bool {
	ch := make(chan bool, 1)
	select {
	case t.gotreply <- gotreply{typ: typ, data: pack, matched: ch}:
		return <-ch
	case <-t.exit:
		return true
	}
}

type (
	ping struct {
		From   endpoint
		To     endpoint
		Expire int64
	}

	pong struct {
		To     endpoint
		Expire int64
	}

	findnode struct {
		FromID string
		Expire int64
	}

	replynode struct {
		Nodes  []*Node
		Expire int64
	}
)

// 数据包
type packet interface {
	handle(t *udp, fromID NodeID, to *net.UDPAddr) error
}

// 处理ping数据包
func (p *ping) handle(t *udp, fromID NodeID, to *net.UDPAddr) error {
	if expire(p.Expire) {
		return Error("ping", errPacketTimeout)
	}

	reply := pong{Expire: time.Now().Add(expirationTime).Unix()}

	log.Println("handle ping", "from", to, ";send pong")
	t.sendMessage(pongPacket, to, &reply)
	log.Println("send ok")
	// if !t.handleReply(pongPacket, p) {
	// 	return errPacketHandle
	// }
	return nil
}

// 处理pong数据包
func (p *pong) handle(t *udp, fromID NodeID, to *net.UDPAddr) error {
	if expire(p.Expire) {
		return Error("pong", errPacketTimeout)
	}

	log.Println("handle pong", "from", to)
	if !t.handleReply(pongPacket, p) {
		return Error("pong", errPacketHandle)
	}
	return nil
}

func (p *findnode) handle(t *udp, fromID NodeID, to *net.UDPAddr) error {
	// todo
	if expire(p.Expire) {
		return Error("findnode", errPacketTimeout)
	}

	log.Println("handle findnode <=", "from", to)
	n := NewNode(fromID, to.IP, to.Port, to.Port)
	t.tab.bondNode(n)
	// 返回reply
	reply := replynode{
		Expire: time.Now().Add(expirationTime).Unix(),
	}

	// 取附近的node
	reply.Nodes = t.tab.closest()
	log.Println("返回节点: ", reply.Nodes)
	t.sendMessage(replynodePacket, to, &reply)
	return nil
}

func (p *replynode) handle(t *udp, fromID NodeID, to *net.UDPAddr) error {
	if expire(p.Expire) {
		return Error("replynode", errPacketTimeout)
	}

	log.Println("handle replynode:", p.Nodes)
	if !t.handleReply(replynodePacket, p) {
		return Error("replynode", errPacketHandle)
	}

	return nil
}

// 编码
func encodePacket(id NodeID, typ byte, pack packet) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(typ)
	// 添加ID
	buf.Write(id[:])
	encoder := json.NewEncoder(buf)
	encoder.Encode(pack)

	return buf.Bytes()
}

// 解码
func decodePacket(buf []byte) (packet, NodeID, error) {
	var pack packet
	typ := buf[0]
	switch typ {
	case pingPacket:
		pack = new(ping)
	case pongPacket:
		pack = new(pong)
	case findnodePacket:
		pack = new(findnode)
	case replynodePacket:
		pack = new(replynode)
	}

	// 获取发送方ID
	var fromID NodeID
	copy(fromID[:], buf[1:17])

	buffer := bytes.NewBuffer(buf[17:])
	decoder := json.NewDecoder(buffer)
	err := decoder.Decode(pack)
	if err != nil {
		return nil, fromID, err
	}
	return pack, fromID, nil
}

func expire(ts int64) bool {
	return false
}

func Error(typ string, err error) error {
	return fmt.Errorf("%s:%v", typ, err)
}

func (t *udp) findnode(to *net.UDPAddr) []*Node {
	var nodes []*Node
	errc := t.addPending(replynodePacket, func(v interface{}) bool {
		// 处理接收到的node
		rn := v.(*replynode)
		log.Println("处理replynode:", rn.Nodes)
		for _, n := range rn.Nodes {
			if n.Validate() {
				nodes = append(nodes, n)
			}
		}
		return true
	})

	p := findnode{
		Expire: time.Now().Add(expirationTime).Unix(),
	}

	log.Println("=> find node:", to)
	t.sendMessage(findnodePacket, to, &p)
	err := <-errc
	if err != nil {
		log.Println("err:", err)
		return nil
	}
	log.Println("find nodes:", nodes)
	return nodes
}

func (t *udp) ping(to *net.UDPAddr) error {
	//t.addPending(pongPacket, func(v interface{}) bool { return true })

	p := ping{
		Expire: time.Now().Add(expirationTime).Unix(),
	}

	errc := t.addPending(pongPacket, func(v interface{}) bool { return true })

	log.Println("ping to", to)
	t.sendMessage(pingPacket, to, &p)

	err := <-errc
	return err
}

func (t *udp) waitping() error {
	return <-t.addPending(pongPacket, func(v interface{}) bool { return true })
}
