package node

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/rpc"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Focinfi/oncekv/cache/master"
	"github.com/Focinfi/oncekv/config"
	"github.com/Focinfi/oncekv/log"
	"github.com/Focinfi/oncekv/utils/mock"
	"github.com/Focinfi/oncekv/utils/urlutil"
	"github.com/gin-gonic/gin"
	"github.com/golang/groupcache"
	"github.com/gorilla/websocket"
)

const (
	jsonHTTPHeader = "application-type/json"
	basePath       = "/oncekv/"
	defaultGroup   = "kv"
	dbGetURLFormat = "%s/i/key/%s"
	logPrefix      = "cache/node:"
)

var (
	// ErrDataNotFound for not found data error
	ErrDataNotFound = fmt.Errorf("%s data not found", logPrefix)
	// ErrDatabaseQueryTimeout for underlying data query timeout error
	ErrDatabaseQueryTimeout = fmt.Errorf("%s upderlying data query timeout", logPrefix)

	dbQueryTimeout  = config.Config.HTTPRequestTimeout
	groupcacheBytes = config.Config.CacheBytes

	httpGetter = mock.HTTPGetter(mock.HTTPGetterFunc(http.Get))
	httpPoster = mock.HTTPPoster(mock.HTTPPosterFunc(http.Post))

	wsUpgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

type masterParam struct {
	Peers []string `json:"peers"`
	DBs   []string `json:"dbs"`
}

// Node for one groupcache server
type Node struct {
	// meta
	sync.RWMutex // protect updating for dbs and peers
	// underlying database
	dbs []string
	// fast db
	fastDB string
	// cache peers
	peers []string
	// master url for update meta(dbs and peers)
	masterAddr      string
	masterRPCClient mock.RPCClient

	// node http server
	*gin.Engine
	httpAddr string

	// groupcache server
	nodeAddr string
	pool     *groupcache.HTTPPool
	group    *groupcache.Group
}

// New returns a new Node with the given info
func New(httpAddr string, nodeAddr string, masterAddr string) *Node {
	cache := &Node{
		masterAddr: strings.TrimSuffix(masterAddr, "/"),
		httpAddr:   httpAddr,
		nodeAddr:   nodeAddr,
		pool:       newPool(nodeAddr),
	}

	cache.Engine = newServer(cache)
	cache.group = newGroup(cache, defaultGroup)

	client, err := rpc.DialHTTP("tcp", masterAddr)
	if err != nil {
		log.Internal.Errorf("fail rpc dialing, err: %v", err)
	}
	cache.masterRPCClient = client

	return cache
}

// Start starts the server
func (node *Node) Start() {
	// try to get meta data
	if err := node.join(); err != nil {
		log.Internal.Fatalf("%s fail join to master, err: %v", logPrefix, err)
	}

	// start the groupcache server
	go func() {
		log.DB.Fatal(logPrefix, http.ListenAndServe(node.nodeAddr, node.pool))
	}()

	// start the node server
	node.Run(node.httpAddr)
}

func newServer(node *Node) *gin.Engine {
	server := gin.Default()
	server.POST("/meta", node.handleMeta)
	server.GET("/stats", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, node.group.Stats)
	})
	server.GET("/key/:key", node.handleGetKey)
	server.GET("/ws/stats", node.handleStatsWebSocket)
	return server
}

func (node *Node) handleStatsWebSocket(ctx *gin.Context) {
	conn, err := wsUpgrader.Upgrade(ctx.Writer, ctx.Request, nil)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}
	defer conn.Close()

	for {
		select {
		case <-time.After(time.Second):
			b, err := json.Marshal(node.group.Stats)
			if err != nil {
				log.DB.Error(err)
				continue
			}

			if err := conn.WriteMessage(1, b); err != nil {
				return
			}
		}
	}
}

func (node *Node) handleGetKey(ctx *gin.Context) {
	result := &groupcache.ByteView{}
	log.DB.Infoln("Start Get")
	err := node.group.Get(ctx.Request.Context(), ctx.Param("key"), groupcache.ByteViewSink(result))
	log.DB.Infoln("End Get")
	if err == ErrDataNotFound {
		ctx.JSON(http.StatusNotFound, nil)
		return
	}

	if err != nil {
		log.DB.Error(logPrefix, err)
		ctx.JSON(http.StatusInternalServerError, nil)
		return
	}

	ctx.Writer.WriteHeader(http.StatusOK)
	ctx.Writer.Header()["Content-Type"] = []string{"application/json; charset=utf-8"}
	ctx.Writer.Write(result.ByteSlice())
}

func newPool(addr string) *groupcache.HTTPPool {
	return groupcache.NewHTTPPoolOpts(urlutil.MakeURL(addr),
		&groupcache.HTTPPoolOptions{
			BasePath: basePath,
		})
}

func newGroup(n *Node, name string) *groupcache.Group {
	return groupcache.NewGroup(name, groupcacheBytes, groupcache.GetterFunc(n.fetchData))
}

func (node *Node) join() error {
	// build join param
	args := &master.JoinParam{
		HTTPAddr: node.httpAddr,
		NodeAddr: node.nodeAddr,
	}
	reply := &master.PeerParam{}
	if err := node.masterRPCClient.Call("Master.JoinNode", args, reply); err != nil {
		return fmt.Errorf("fail call Master.JoinNode, err: %v", err)
	}
	log.Internal.Infof("%s join reply: %v", logPrefix, reply)

	node.Lock()
	defer node.Unlock()

	// update meta
	node.pool.Set(reply.Peers...)
	node.peers = reply.Peers
	node.dbs = reply.DBs
	return nil
}

func (node *Node) handleMeta(ctx *gin.Context) {
	params := masterParam{}
	if err := ctx.BindJSON(&params); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sort.StringSlice(params.Peers).Sort()
	sort.StringSlice(params.DBs).Sort()

	node.RLock()
	if reflect.DeepEqual(node.peers, params.Peers) &&
		reflect.DeepEqual(node.dbs, params.DBs) {

		node.RUnlock()
		// return if no changes
		ctx.JSON(http.StatusOK, nil)
		return
	}
	node.RUnlock()

	log.Biz.Infof("%s [peers] local:%#v, remote: %#v\n", logPrefix, node.peers, params.Peers)
	log.Biz.Infof("%s [dbs] local:%#v, remote: %#v\n", logPrefix, node.dbs, params.DBs)

	node.Lock()
	defer node.Unlock()

	node.pool.Set(params.Peers...)
	node.peers = params.Peers
	node.dbs = params.DBs

	ctx.JSON(http.StatusOK, nil)
}

func (node *Node) fetchData(ctx groupcache.Context, key string, dest groupcache.Sink) error {
	if node.fastDB == "" {
		return node.tryAllDBFind(ctx, key, dest)
	}

	data, err := node.find(key, node.fastDB)
	if err == ErrDataNotFound {
		return err
	}

	if err != nil {
		log.DB.Error(logPrefix, err)
		go node.setFastDB("")
		return node.tryAllDBFind(ctx, key, dest)
	}

	dest.SetBytes(data)
	return nil
}

func (node *Node) find(key string, url string) ([]byte, error) {
	url = fmt.Sprintf(dbGetURLFormat, urlutil.MakeURL(url), key)
	resp, err := httpGetter.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrDataNotFound
	}

	if resp.StatusCode == http.StatusOK {
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		if len(b) == 0 {
			return nil, fmt.Errorf("%s database error, lost data of key: %s\n", logPrefix, key)
		}

		return b, nil
	}

	return nil, fmt.Errorf("%s failed to fetch data", logPrefix)
}

func (node *Node) tryAllDBFind(ctx groupcache.Context, key string, dest groupcache.Sink) error {
	dbs := make([]string, len(node.dbs))
	copy(dbs, node.dbs)
	log.Biz.Infoln(logPrefix, "start fetchData:", time.Now(), dbs)
	if len(dbs) == 0 {
		return fmt.Errorf("%s databases are not available\n", logPrefix)
	}

	var got bool
	var data = make(chan []byte)
	var completeCount int
	var fastURL string
	var resErr error

	for _, db := range dbs {
		go func(url string) {
			val, err := node.find(key, url)

			node.Lock()
			defer node.Unlock()
			if len(val) > 0 || err == ErrDataNotFound || completeCount == len(dbs) {
				if !got {
					got = true
					fastURL = url
					resErr = err

					go func() { data <- val }()
				}
			}
		}(db)
	}

	select {
	case <-time.After(dbQueryTimeout):
		go node.setFastDB("")
		return ErrDatabaseQueryTimeout

	case value := <-data:
		log.Biz.Infoln(logPrefix, "end get:", time.Now())
		dest.SetBytes(value)

		if len(value) > 0 || resErr == ErrDataNotFound {
			go node.setFastDB(fastURL)
		}

		return resErr
	}
}

func (node *Node) setFastDB(db string) {
	node.Lock()
	defer node.Unlock()

	node.fastDB = db
}
