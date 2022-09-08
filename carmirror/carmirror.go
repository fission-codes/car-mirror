package carmirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fission-codes/go-car-mirror/dag"
	gocid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	golog "github.com/ipfs/go-log"
	mdag "github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-merkledag/traverse"
	coreiface "github.com/ipfs/interface-go-ipfs-core"
	"github.com/ipfs/interface-go-ipfs-core/path"
	carv1 "github.com/ipld/go-car"
	"github.com/pkg/errors"
)

var log = golog.Logger("car-mirror")

const (
	CarMirrorProtocolID = "/car-mirror/" + Version
	sessionIdHeader     = "car-mirror-sid"
)

var (
	ErrUnknownProtocolVersion = fmt.Errorf("unknown protocol version")
)

// or pushable, pullable?
type CarMirrorable interface {
	Push(ctx context.Context, cids []gocid.Cid, diff string) (err error)
	Pull(ctx context.Context, cids []gocid.Cid) (err error)
	// NewSession() (id string, err error)
}

type CarMirror struct {
	// Local node getter
	lng ipld.NodeGetter

	// CoreAPI
	capi coreiface.CoreAPI

	// Local block API
	bapi coreiface.BlockAPI

	// HTTP server accepting CAR Mirror requests
	httpServer *http.Server

	sessionLock    sync.Mutex
	sessionPool    map[string]*session
	sessionCancels map[string]context.CancelFunc
	sessionTTLDur  time.Duration
}

var (
// compile-time assertion that CarMirror satisfies the remote interface
// _ CarMirrorable = (*CarMirror)(nil)
)

// Config encapsulates CAR Mirror configuration
type Config struct {
	HTTPRemoteAddr string
}

// Validate confirms the configuration is valid
func (cfg *Config) Validate() error {
	if cfg.HTTPRemoteAddr == "" {
		return fmt.Errorf("HTTPRemoteAddr is required")
	}

	return nil
}

// New creates a local CAR Mirror service.
//
// Its crucial that the NodeGetter passed to New be an offline-only getter.
// If using IPFS, this package defines a helper function: NewLocalNodeGetter
// to get an offline-only node getter from an IPFS CoreAPI interface.
func New(localNodes ipld.NodeGetter, capi coreiface.CoreAPI, blockStore coreiface.BlockAPI, opts ...func(cfg *Config)) (*CarMirror, error) {
	// Add default stuff to the config
	cfg := &Config{}

	for _, opt := range opts {
		opt(cfg)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	cm := &CarMirror{
		lng:  localNodes,
		capi: capi,
		bapi: blockStore,

		sessionPool:    map[string]*session{},
		sessionCancels: map[string]context.CancelFunc{},
		// Spec: The Provider MAY garbage collect its session state when it has exhausted its graph, since false positives in the Bloom filter MAY lead to the Provider having an incorrect picture of the Requestor's store. In addition, further requests MAY come in for that session. Session state is an optimization, so treating this as a totally new session is acceptable. However, due to this fact, it is RECOMMENDED that the Provider maintain a session state TTL of at least 30 seconds since the last block is sent. Maintaining this cache for long periods can speed up future requests, so the Provider MAY keep this information around to aid future requests.
		sessionTTLDur: time.Second * 30,
	}

	if cfg.HTTPRemoteAddr != "" {
		m := http.NewServeMux()
		m.Handle("/dag/push", cm.HTTPRemotePushHandler())
		m.Handle("/dag/pull", cm.HTTPRemotePullHandler())

		cm.httpServer = &http.Server{
			Addr:    cfg.HTTPRemoteAddr,
			Handler: m,
		}
	}

	return cm, nil
}

// StartRemote makes car mirror available for remote requests, starting an HTTP
// server if a listening address is specified.
// StartRemote returns immediately. Stop remote service by cancelling
// the passed-in context.
func (cm *CarMirror) StartRemote(ctx context.Context) error {
	if cm.httpServer == nil {
		return fmt.Errorf("CAR Mirror is not configured as a remote")
	}

	go func() {
		<-ctx.Done()
		if cm.httpServer != nil {
			cm.httpServer.Close()
		}
	}()

	if cm.httpServer != nil {
		go cm.httpServer.ListenAndServe()
	}

	log.Debug("CAR Mirror remote started")
	return nil
}

func (cm *CarMirror) mirrorableRemote(remoteAddr string) (rem CarMirrorable, err error) {
	if strings.HasPrefix(remoteAddr, "http") {
		rem = &HTTPClient{URL: remoteAddr, NodeGetter: cm.lng, BlockAPI: cm.bapi}
	} else {
		return nil, fmt.Errorf("unrecognized remote address string: %s", remoteAddr)
	}

	return rem, nil
}

// NewPush creates a push to a remote address
func (cm *CarMirror) NewPush(ctx context.Context, cidStr, remoteAddr string, diff string, stream bool) (*Push, error) {
	cids, err := cm.GetLocalCids(ctx, cidStr)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get local cids for cid %v", cidStr)
	}

	rem, err := cm.mirrorableRemote(remoteAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get mirrorable remote for addr %v", remoteAddr)
	}

	return NewPush(cm.lng, cids, diff, rem, stream), nil
}

func (cm *CarMirror) NewSession() (sid string, err error) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(cm.sessionTTLDur))

	sess, err := newSession(ctx, cm.lng, cm.bapi)
	if err != nil {
		cancel()
		return
	}

	cm.sessionLock.Lock()
	defer cm.sessionLock.Unlock()
	cm.sessionPool[sess.id] = sess
	cm.sessionCancels[sess.id] = cancel

	return sess.id, nil
}

func (cm *CarMirror) finalizeMirror(sess *session) error {
	log.Debug("finalizing mirror session", sess.id)

	defer func() {
		cm.sessionLock.Lock()
		cm.sessionCancels[sess.id]()
		delete(cm.sessionPool, sess.id)
		cm.sessionLock.Unlock()
	}()

	return nil
}

type PushParams struct {
	Cid    string
	Addr   string
	Diff   string
	Stream bool
}

type PullParams struct {
	Cid    string
	Addr   string
	Stream bool
}

// NewPush creates a push to a remote address
func (cm *CarMirror) NewPull(ctx context.Context, cidStr, remoteAddr string, stream bool) (*Pull, error) {
	id, err := gocid.Parse(cidStr)
	if err != nil {
		return nil, err
	}
	cids := []gocid.Cid{id}

	rem, err := cm.mirrorableRemote(remoteAddr)
	if err != nil {
		return nil, err
	}

	return NewPull(cm.lng, cids, rem, stream), nil
}

// TODO: Rename to indicate this is really a new push session handler
func (cm *CarMirror) NewPushSessionHandler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			p := PushParams{
				Cid:    r.FormValue("cid"),
				Addr:   r.FormValue("addr"),
				Diff:   r.FormValue("diff"),
				Stream: r.FormValue("stream") == "true",
			}

			log.Infof("performing push:\n\tcid: %s\n\taddr: %s\n\tdiff: %s\n\tstream: %v\n", p.Cid, p.Addr, p.Diff, p.Stream)

			sid, err := cm.NewSession()
			if err != nil {
				log.Debugf("error creating session: %s", err.Error())
				w.Write([]byte(err.Error()))
				return
			}
			w.Header().Set(sessionIdHeader, sid)

			// Would it make sense to treat this like an iterator, looping through requests until there is no more?
			// NewX
			// Next
			// Value
			// pusher := NewPusher(...)
			// for pusher.Next() {
			// 	nextPush := pusher.Value()
			//  nextPush.Do(...)?
			// }
			// Return ...

			// Need list of cids here, since protocol takes list.
			// so move getting list of cids to this level

			// Create the initial push with the list of cids
			// We will be widdling this list down
			push, err := cm.NewPush(r.Context(), p.Cid, p.Addr, p.Diff, p.Stream)
			if err != nil {
				w.Write([]byte(err.Error()))
				return
			}

			if err = push.Do(r.Context()); err != nil {
				log.Debugf("push error: %s", err.Error())
				w.Write([]byte(err.Error()))
				return
			}

			data, err := json.Marshal(p)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
			}

			// Write out JSON encoded params for the request
			w.Header().Add("Content-Type", "application/json")
			w.Write(data)
		}
	})
}

func (cm *CarMirror) NewPullSessionHandler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			p := PullParams{
				Cid:    r.FormValue("cid"),
				Addr:   r.FormValue("addr"),
				Stream: r.FormValue("stream") == "true",
			}

			log.Infof("performing pull:\n\tcid: %s\n\taddr: %s\n\tdiff: %s\n\tstream: %v\n", p.Cid, p.Addr, p.Stream)

			pull, err := cm.NewPull(r.Context(), p.Cid, p.Addr, p.Stream)
			if err != nil {
				fmt.Printf("error creating pull: %s\n", err.Error())
				w.Write([]byte(err.Error()))
				return
			}

			if err = pull.Do(r.Context()); err != nil {
				fmt.Printf("pull error: %s\n", err.Error())
				w.Write([]byte(err.Error()))
				return
			}

			fmt.Println("pull complete")

			data, err := json.Marshal(p)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
			}

			// Write out JSON encoded params for the request
			w.Header().Add("Content-Type", "application/json")
			w.Write(data)
		}
	})
}

// GetLocalCids returns a unique list of `gocid.Cid`s underneath a given root CID, using an offline CoreAPI.
// The root CID is included in the returned list.
// In the case of an error, both the discovered CIDs thus far and the error are returned.
func (cm *CarMirror) GetLocalCids(ctx context.Context, rootCidStr string) ([]gocid.Cid, error) {
	var cids []gocid.Cid
	rootCid, err := dag.ParseCid(rootCidStr)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse root cid %s", rootCidStr)
	}
	cids = append(cids, *rootCid)

	rp, err := cm.capi.ResolvePath(ctx, path.New(rootCidStr))
	if err != nil {
		return cids, errors.Wrapf(err, "unable to resolve path for root cid %s", rootCidStr)
	}

	nodeGetter := mdag.NewSession(ctx, cm.lng)
	obj, err := nodeGetter.Get(ctx, rp.Cid())
	if err != nil {
		return cids, errors.Wrapf(err, "unable to get nodes for root cid %s", rootCidStr)
	}
	err = traverse.Traverse(obj, traverse.Options{
		DAG:   nodeGetter,
		Order: traverse.DFSPre,
		Func: func(current traverse.State) error {
			cids = append(cids, current.Node.Cid())
			return nil
		},
		ErrFunc:        nil,
		SkipDuplicates: true,
	})
	if err != nil {
		return cids, errors.Wrapf(err, "error traversing DAG: %v", err)
	}

	return cids, nil
}

func (cm *CarMirror) WriteCar(ctx context.Context, cids []gocid.Cid, w io.Writer) error {
	return carv1.WriteCar(ctx, cm.lng, cids, w)
}
