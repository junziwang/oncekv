package master

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/rpc"
	"sort"
	"sync"
	"time"

	"github.com/Focinfi/oncekv/config"
	"github.com/Focinfi/oncekv/db/master"
	"github.com/Focinfi/oncekv/log"
	"github.com/Focinfi/oncekv/meta"
	"github.com/Focinfi/oncekv/utils/mock"
	"github.com/Focinfi/oncekv/utils/urlutil"
)

const (
	jsonHTTPHeader     = "application/json"
	heartbeatURLFormat = "%s/meta"
	logPrefix          = "cache/master:"
)

var (
	defaultHeartbeatPeriod = time.Second
	defaultAddr            = config.Config.CacheMasterAddr
	cacheNodesKey          = config.Config.CacheNodesKey
	httpPoster             = mock.HTTPPoster(mock.HTTPPosterFunc(http.Post))
)

// nodesMap is pairs of httpAddr/nodeAddr
type nodesMap map[string]string

func (p nodesMap) httpAddrs() []string {
	addrs := make([]string, len(p))
	i := 0
	for k := range p {
		addrs[i] = k
		i++
	}

	sort.StringSlice(addrs).Sort()
	return addrs
}

func (p nodesMap) nodeAddrs() []string {
	addrs := make([]string, len(p))
	i := 0
	for k := range p {
		addrs[i] = p[k]
		i++
	}

	sort.StringSlice(addrs).Sort()
	return addrs
}

type PeerParam struct {
	Peers []string `json:"peers"`
	DBs   []string `json:"dbs"`
}

type JoinParam struct {
	HTTPAddr string `json:"httpAddr"`
	NodeAddr string `json:"nodeAddr"`
}

// Master for a group of caching nodes
type Master struct {
	// runtime data
	sync.RWMutex
	nodesMap nodesMap
	dbs      []string

	// http server
	addr string

	// database store
	meta        meta.Meta
	nodesMapKey string
}

// Default returns a new Master with the default addr
var Default = New(defaultAddr)

// New returns a new Master with the addr
func New(addr string) *Master {
	m := &Master{
		addr:        addr,
		nodesMapKey: cacheNodesKey,
		meta:        meta.Default,
	}

	nodesMap, err := m.fetchNodesMap()
	if err != nil {
		panic(err)
	}

	log.Biz.Infoln(logPrefix, "Nodes: ", nodesMap)

	m.nodesMap = nodesMap
	return m
}

// Start starts the master listening on addr
func (m *Master) Start() {
	go m.meta.WatchModify(m.nodesMapKey, func() { m.syncDBs() })
	go m.heartbeat()

	rpc.Register(m)
	rpc.HandleHTTP()
	l, e := net.Listen("tcp", m.addr)
	if e != nil {
		log.Internal.Fatal("listen error:", e)
	}
	go http.Serve(l, nil)
}

// Peers returns the httpAddrs
func (m *Master) Peers() ([]string, error) {
	peers, err := m.fetchNodesMap()
	if err != nil {
		return nil, err
	}

	return peers.httpAddrs(), nil
}

func (m *Master) setNodesMap(peers nodesMap) {
	m.Lock()
	defer m.Unlock()

	m.nodesMap = peers
}

func (m *Master) JoinNode(args *JoinParam, reply *PeerParam) error {
	if args == nil || args.HTTPAddr == "" || args.NodeAddr == "" {
		return fmt.Errorf("%s wrong params", logPrefix)
	}

	m.Lock()
	m.nodesMap[urlutil.MakeURL(args.HTTPAddr)] = urlutil.MakeURL(args.NodeAddr)
	if err := m.updateNodesMap(m.nodesMap); err != nil {
		m.Unlock()
		return fmt.Errorf("%s fail updateNodesMap, err: %v", logPrefix, err)
	}

	*reply = PeerParam{Peers: m.nodesMap.httpAddrs(), DBs: m.dbs}
	m.Unlock()
	log.DB.Infoln("join:", m.nodesMap)
	return nil
}

func (m *Master) fetchNodesMap() (nodesMap, error) {
	nodes := nodesMap{}

	val, err := m.meta.Get(m.nodesMapKey)
	if err == config.ErrDataNotFound {
		return nodes, nil
	}

	if err := json.Unmarshal([]byte(val), &nodes); err != nil {
		return nodes, err
	}

	return nodes, nil
}

func (m *Master) updateNodesMap(peers nodesMap) error {
	b, err := json.Marshal(peers)
	if err != nil {
		return err
	}

	return m.meta.Put(m.nodesMapKey, string(b))
}

// heartbeat for check the nodes health periodicly
func (m *Master) heartbeat() {
	ticker := time.NewTicker(defaultHeartbeatPeriod)
	for {
		<-ticker.C
		m.RLock()
		nodesMap := m.nodesMap
		m.RUnlock()

		nodePeers := nodesMap.nodeAddrs()
		for _, nodeURL := range nodesMap.httpAddrs() {
			go func(node string) {
				err := m.sendPeers(node, nodePeers)
				if err != nil {
					log.Internal.Errorln(logPrefix, "node error:", err)
					m.removeNode(node)
				}
			}(nodeURL)
		}
	}
}

func (m *Master) sendPeers(node string, nodes []string) error {
	if err := m.syncDBs(); err != nil {
		return err
	}

	params := PeerParam{Peers: nodes, DBs: m.dbs}

	b, err := json.Marshal(&params)
	if err != nil {
		return err
	}

	res, err := httpPoster.Post(fmt.Sprintf(heartbeatURLFormat, urlutil.MakeURL(node)), jsonHTTPHeader, bytes.NewReader(b))

	if err != nil {
		return err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return errors.New("failed to send peers")
	}

	return err
}

func (m *Master) removeNode(node string) {
	m.Lock()
	if _, ok := m.nodesMap[node]; !ok {
		m.Unlock()
		return
	}

	delete(m.nodesMap, node)

	if err := m.updateNodesMap(m.nodesMap); err != nil {
		log.DB.Errorln(logPrefix, "database error:", err)
	}

	log.DB.Errorln(logPrefix, node, "removed")
	m.Unlock()
}

func (m *Master) syncDBs() error {
	dbs, err := master.Default.Peers()
	if err != nil {
		return err
	}

	m.Lock()
	defer m.Unlock()
	m.dbs = dbs

	return nil
}
