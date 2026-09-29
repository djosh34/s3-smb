package smb2

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djosh34/s3-smb/internal/smb2/internal/crypto/ccm"
	"github.com/djosh34/s3-smb/internal/smb2/internal/crypto/cmac"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
	"golang.org/x/exp/maps"
)

const DEFAULT_IOPS = 32

type Server struct {
	maxCreditBalance uint16 // if it's zero, clientMaxCreditBalance is used. (See feature.go for more details)
	negotiator       ServerNegotiator
	authenticator    Authenticator

	serverStartTime time.Time
	serverGuid      Guid

	listener net.Listener
	active   atomic.Bool

	shares     map[string]vfs.VFSFileSystem
	origShares map[string]vfs.VFSFileSystem

	opens         map[uint64]*Open
	opensByGuid   map[Guid]*Open
	deletePending map[uint64]bool

	allowGuest bool

	maxIOReads  int
	maxIOWrites int

	xattrs bool

	ignoreSetAttrErr          bool
	enableSMB3PosixExtensions bool

	activeConns map[*conn]struct{}
	connWG      sync.WaitGroup
	cleanupErr  error
	stopping    bool

	acceptSingleConn bool

	lock     sync.Mutex
	lockCond *sync.Cond
	xattrMu  sync.Mutex // serializes native xattr mutations across handles/sessions
}

type OpLockState uint8

const (
	LOCKSTATE_NONE OpLockState = iota
	LOCKSTATE_HELD
	LOCKSTATE_BREAKING
)

type Open struct {
	fileId                      uint64
	durableFileId               uint64
	session                     *session
	tree                        *treeConn
	grantedAccess               uint32
	oplockLevel                 uint8
	oplockState                 OpLockState
	oplockTimeout               time.Duration
	isDurable                   bool
	durableOpenTimeout          time.Duration
	durableOpenScavengerTimeout time.Duration
	durableOwner                uint64
	currentEaIndex              uint32
	currentQuotaIndex           uint32
	lockCount                   int
	byteRangeLocks              []smbByteRangeLock
	pathName                    string
	fileName                    string
	resumeKey                   [24]byte
	createOptions               uint32
	deleteOnClose               bool
	createDisposition           uint32
	fileAttributes              uint32
	clientGuid                  Guid
	lease                       *Lease
	isResilient                 bool
	resiliencyTimeout           time.Duration
	resilientOpenTimeout        time.Duration
	lockSequenceArray           [64]byte
	notifyReq                   []byte
	notifyReqAsyncId            uint64
	queryDirectoryPending       []vfs.DirInfo
	queryDirectorySingleName    string
	queryDirectorySingleDone    bool
	isEa                        bool
	eaKey                       string
	isSymlink                   bool
	posixSemantics              bool
	createGuid                  Guid
	appInstanceId               Guid
	isPersistent                bool
	channelSequence             uint16
	outstandingRequestCoun      int
	outstandingPreRequestCount  int
}

type Lease struct {
	LeaseKey       Guid
	LeaseState     uint32
	LeaseFlags     uint32
	LeaseDuration  uint64
	ParentLeaseKey Guid
	Epoch          uint16
	Version        int
	Breaking       bool
	BreakToState   uint32
}

// Negotiator contains options for func (*Dialer) Dial.
type ServerNegotiator struct {
	RequireMessageSigning bool   // enforce signing?
	SpecifiedDialect      uint16 // if it's zero, clientDialects is used. (See feature.go for more details)
	Spnego                *spnegoServer
}

type ConnState int

const (
	STATE_NEGOTIATE = ConnState(iota)
	STATE_SESSION_SETUP
	STATE_SESSION_SETUP_CHALLENGE
	STATE_SESSION_ACTIVE
)

var (
	SRVSVC_GUID  = FileId{}
	INVALID_GUID = FileId{
		Persistent: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		Volatile:   [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
)

type ServerConfig struct {
	AllowGuest                bool
	MaxIOReads                int
	MaxIOWrites               int
	Xatrrs                    bool
	IgnoreSetAttrErr          bool
	AcceptSingleConn          bool
	EnableSMB3PosixExtensions bool
}

func NewServer(cfg *ServerConfig, a Authenticator, shares map[string]vfs.VFSFileSystem) *Server {
	newShares := map[string]vfs.VFSFileSystem{}
	for i, v := range shares {
		newShares[strings.ToUpper(i)] = v
	}

	srv := &Server{
		authenticator:             a,
		shares:                    newShares,
		origShares:                shares,
		opens:                     map[uint64]*Open{},
		opensByGuid:               map[Guid]*Open{},
		deletePending:             map[uint64]bool{},
		allowGuest:                cfg.AllowGuest,
		maxIOReads:                cfg.MaxIOReads,
		maxIOWrites:               cfg.MaxIOWrites,
		xattrs:                    cfg.Xatrrs,
		ignoreSetAttrErr:          cfg.IgnoreSetAttrErr,
		enableSMB3PosixExtensions: cfg.EnableSMB3PosixExtensions,
		activeConns:               map[*conn]struct{}{},
		acceptSingleConn:          cfg.AcceptSingleConn,
	}
	srv.lockCond = sync.NewCond(&srv.lock)
	return srv
}

func (d *Server) AddShare(name string, fs vfs.VFSFileSystem) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("share name is required")
	}
	if fs == nil {
		return fmt.Errorf("share filesystem is required")
	}
	upperName := strings.ToUpper(name)
	d.lock.Lock()
	defer d.lock.Unlock()
	if _, ok := d.shares[upperName]; ok {
		return fmt.Errorf("share %q already exists", name)
	}
	d.shares[upperName] = fs
	d.origShares[name] = fs
	return nil
}

func (d *Server) RemoveShare(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("share name is required")
	}
	upperName := strings.ToUpper(name)
	d.lock.Lock()
	defer d.lock.Unlock()
	if _, ok := d.shares[upperName]; !ok {
		return fmt.Errorf("share %q does not exist", name)
	}
	delete(d.shares, upperName)
	for existing := range d.origShares {
		if strings.EqualFold(existing, name) {
			delete(d.origShares, existing)
			break
		}
	}
	return nil
}

func (d *Server) Shares() []string {
	d.lock.Lock()
	defer d.lock.Unlock()
	return maps.Keys(d.origShares)
}

// Serve listens on the given address and serves SMB connections.
func (d *Server) Serve(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	return d.ServeListener(listener)
}

// ServeListener serves SMB connections on the given listener.
func (d *Server) ServeListener(listener net.Listener) error {
	_, err := rand.Read(d.serverGuid[:])
	if err != nil {
		log.Errorf("failed to generate server guid")
		return &InternalError{err.Error()}
	}
	rand.Read(SRVSVC_GUID.Persistent[:])
	rand.Read(SRVSVC_GUID.Volatile[:])

	d.lock.Lock()
	if d.stopping {
		d.lock.Unlock()
		listener.Close()
		return net.ErrClosed
	}
	d.listener = listener
	d.active.Store(true)
	d.lock.Unlock()
	defer listener.Close()

	for d.active.Load() {
		// Accept a new connection.
		c, err := listener.Accept()
		if err != nil {
			if !d.active.Load() {
				return nil
			}
			return err
		}

		ctx, cancel := context.WithCancel(context.Background())

		maxCreditBalance := d.maxCreditBalance
		if maxCreditBalance == 0 {
			maxCreditBalance = clientMaxCreditBalance
		}
		a := openAccount(maxCreditBalance)

		conn := &conn{
			t:                   direct(c),
			outstandingRequests: newOutstandingRequests(),
			account:             a,
			rdone:               make(chan struct{}, 1),
			wdone:               make(chan struct{}, 1),
			write:               make(chan []byte, 10),
			werr:                make(chan error, 1),
			ctx:                 ctx,
			cancel:              cancel,
			serverCtx:           d,
			serverState:         STATE_NEGOTIATE,
			cipherId:            AES128GCM,
			hashId:              SHA512,
			treeMapByName:       make(map[string]treeOps),
			treeMapById:         make(map[uint32]treeOps),
			sessions:            make(map[uint64]*session),
		}

		d.lock.Lock()
		log.Debugf("activeConn :%d, accept more: %v", len(d.activeConns), d.acceptSingleConn)
		if len(d.activeConns) > 0 && d.acceptSingleConn {
			accept := true
			for c := range d.activeConns {
				if c.serverState == STATE_SESSION_ACTIVE {
					accept = false
					break
				}
			}
			if !accept {
				d.lock.Unlock()
				conn.shutdown()
				continue
			}
		}

		if d.stopping {
			d.lock.Unlock()
			conn.shutdown()
			return nil
		}
		d.activeConns[conn] = struct{}{}
		d.connWG.Add(1)
		d.lock.Unlock()
		conn.transportWG.Add(2)
		go func() { defer conn.transportWG.Done(); conn.runReciever() }()
		go func() { defer conn.transportWG.Done(); conn.runSender() }()

		run := func() {
			if err := conn.Run(); err != nil {
				log.Errorf("err: %v", err)
				c.Close()
				if d.acceptSingleConn && conn.serverState == STATE_SESSION_ACTIVE {
					d.active.Store(false)
					listener.Close()
				}
			}
		}
		// Handle the connection in a new goroutine.
		go func() {
			defer d.connWG.Done()
			run()
			conn.shutdown()
			err := conn.closeTreeHandles(nil)
			conn.lockWG.Wait()
			conn.transportWG.Wait()
			d.lock.Lock()
			d.cleanupErr = errors.Join(d.cleanupErr, err)
			delete(d.activeConns, conn)
			d.lock.Unlock()
		}()

	}
	return nil
}

func (d *Server) Shutdown() {
	d.lock.Lock()
	d.stopping = true
	d.active.Store(false)
	if d.listener != nil {
		d.listener.Close()
	}
	conns := make([]*conn, 0, len(d.activeConns))
	for c := range d.activeConns {
		conns = append(conns, c)
	}
	d.lock.Unlock()
	for _, c := range conns {
		c.shutdown()
	}
}

func (c *conn) Run() error {
	for {
		pkt, reqSession, compCtx, err := c.srvRecv()
		if err != nil {
			return err
		}
		if reqSession != nil && c.session != reqSession {
			c.session = reqSession
		}

		p := PacketCodec(pkt)
		if compCtx != nil && p.Flags()&SMB2_FLAGS_RELATED_OPERATIONS == 0 {
			compCtx.treeId, compCtx.sessionId = uint64(p.TreeId()), p.SessionId()
			compCtx.fileId, compCtx.lastStatus = nil, 0
		}
		if p.Command() != SMB2_NEGOTIATE && p.Command() != SMB_COM_NEGOTIATE && p.Command() != SMB2_SESSION_SETUP && p.Command() != SMB2_ECHO && (!c.useSession() || reqSession == nil) {
			rsp := new(ErrorResponse)
			PrepareResponse(rsp.Header(), pkt, uint32(STATUS_USER_SESSION_DELETED))
			if err := c.sendPacket(rsp, nil, compCtx); err != nil {
				return err
			}
			continue
		}
		if p.Flags()&SMB2_FLAGS_ASYNC_COMMAND != 0 {
			log.Debugf("Async command %d", p.Command())
		}

		switch p.Command() {
		case SMB2_NEGOTIATE, SMB_COM_NEGOTIATE:
			err = c.negotiate(pkt)
		case SMB2_SESSION_SETUP:
			err = c.sessionSetup(pkt)
		case SMB2_LOGOFF:
			err = c.logoff(pkt)
		case SMB2_TREE_CONNECT:
			err = c.treeConnect(pkt)
		case SMB2_TREE_DISCONNECT:
			err = c.treeDisconnect(pkt)
		case SMB2_ECHO:
			err = c.echo(compCtx, pkt)
		case SMB2_CANCEL:
			err = nil
		default:
			p := PacketCodec(pkt)
			treeID := p.TreeId()
			if compCtx != nil && p.Flags()&SMB2_FLAGS_RELATED_OPERATIONS != 0 {
				treeID = uint32(compCtx.treeId)
			}
			tc, ok := c.treeMapById[treeID]
			if !ok {
				err = &InvalidRequestError{fmt.Sprintf("tree %d doesn't exist: command %d", p.TreeId(), p.Command())}
				break
			}

			// if prev transaction req failed, don't forward it
			if compCtx != nil && compCtx.lastStatus != 0 {
				rsp := new(ErrorResponse)
				PrepareResponse(&rsp.PacketHeader, pkt, uint32(compCtx.lastStatus))
				c.sendPacket(rsp, tc.getTree(), compCtx)
				continue
			}

			if tree, ok := tc.(*fileTree); ok {
				if status := tree.validateRequest(compCtx, pkt); status != STATUS_SUCCESS {
					rsp := new(ErrorResponse)
					PrepareResponse(rsp.Header(), pkt, uint32(status))
					if err := c.sendPacket(rsp, tree.getTree(), compCtx); err != nil {
						return err
					}
					continue
				}
			}
			switch p.Command() {
			case SMB2_CREATE:
				err = tc.create(compCtx, pkt)
			case SMB2_CLOSE:
				err = tc.close(compCtx, pkt)
			case SMB2_FLUSH:
				err = tc.flush(compCtx, pkt)
			case SMB2_READ:
				err = tc.read(compCtx, pkt)
			case SMB2_WRITE:
				err = tc.write(compCtx, pkt)
			case SMB2_LOCK:
				err = tc.lock(compCtx, pkt)
			case SMB2_IOCTL:
				err = tc.ioctl(compCtx, pkt)
			case SMB2_CANCEL:
				err = tc.cancel(compCtx, pkt)
			case SMB2_QUERY_DIRECTORY:
				err = tc.queryDirectory(compCtx, pkt)
			case SMB2_CHANGE_NOTIFY:
				err = tc.changeNotify(compCtx, pkt)
			case SMB2_QUERY_INFO:
				err = tc.queryInfo(compCtx, pkt)
			case SMB2_SET_INFO:
				err = tc.setInfo(compCtx, pkt)
			case SMB2_OPLOCK_BREAK:
				err = tc.oplockBreak(compCtx, pkt)
			}
		}
		if err != nil {
			log.Errorf("err: %v", err)
			return err
		}
	}
}

func (c *conn) echo(ctx *compoundContext, pkt []byte) error {
	log.Debugf("Echo")

	p := PacketCodec(pkt)
	rsp := new(EchoResponse)

	rsp.CreditRequestResponse = p.CreditRequest()
	rsp.MessageId = p.MessageId()
	rsp.Flags = 1
	rsp.SessionId = p.SessionId()

	return c.sendPacket(rsp, nil, nil)
}

func (c *conn) negotiate(pkt []byte) error {
	log.Debugf("Negotiate")

	if c.serverState != STATE_NEGOTIATE {
		if c.useSession() {
			c.session = nil
			c.resetSession()
			c.serverState = STATE_NEGOTIATE
		}
	}

	// Negotiation chooses a dialect per connection, not for the whole server.
	negotiator := c.serverCtx.negotiator
	return negotiator.negotiate(c, pkt)
}

func (c *conn) sessionSetup(pkt []byte) error {
	log.Debugf("SessionSetup")

	if c.useSession() {
		c.session = nil
		c.resetSession()
		c.serverState = STATE_NEGOTIATE
	}

	switch c.serverState {
	case STATE_NEGOTIATE:
		return c.sessionServerSetup(pkt)
	case STATE_SESSION_SETUP, STATE_SESSION_SETUP_CHALLENGE:
		return c.sessionServerSetupChallenge(pkt)
	default:
		break
	}

	log.Debugf("Wrong connection state %d", c.serverState)
	return &InvalidRequestError{"wrong connction state"}
}

func (c *conn) logoff(pkt []byte) error {
	log.Debugf("Logoff")

	p := PacketCodec(pkt)
	if err := c.closeTreeHandles(nil); err != nil {
		rsp := new(ErrorResponse)
		PrepareResponse(rsp.Header(), pkt, uint32(statusFromError(err)))
		return c.sendPacket(rsp, nil, nil)
	}
	rsp := new(LogoffResponse)

	rsp.CreditRequestResponse = p.CreditRequest()
	rsp.MessageId = p.MessageId()
	rsp.Flags = 1

	err := c.sendPacket(rsp, nil, nil)
	c.shutdown() // this server exposes one authenticated session per connection
	return err
}

func (c *conn) treeConnect(pkt []byte) error {
	log.Debugf("TreeConnect")

	p := PacketCodec(pkt)

	res, err := accept(SMB2_TREE_CONNECT, pkt)
	if err != nil {
		return err
	}

	r := TreeConnectRequestDecoder(res)
	if r.IsInvalid() {
		return &InvalidResponseError{"broken tree connect format"}
	}

	log.Debugf("TreeConnect: %s", r.Path())

	rsp := new(TreeConnectResponse)
	rsp.CreditRequestResponse = p.CreditRequest()
	rsp.MessageId = p.MessageId()
	rsp.Flags = 1

	if strings.HasSuffix(r.Path(), "\\IPC$") {
		rsp.ShareType = SMB2_SHARE_TYPE_PIPE
		rsp.MaximalAccess = SYNCHRONIZE | WRITE_OWNER | WRITE_DAC | READ_CONTROL | DELETE |
			FILE_READ_ATTRIBUTES | FILE_WRITE_ATTRIBUTES |
			FILE_EXECUTE | FILE_READ_EA | FILE_WRITE_EA |
			FILE_READ_DATA | FILE_WRITE_DATA | FILE_DELETE_CHILD

		var tc *treeConn
		if t, ok := c.treeMapByName["\\IPC$"]; ok {
			rsp.TreeId = t.getTree().treeId
			tc = t.getTree()
			tc.refCount++
		} else {
			shares := c.serverCtx.Shares()
			ft := &ipcTree{
				treeConn: treeConn{
					session:    c.session,
					treeId:     randint32(),
					shareFlags: 0,
					path:       "\\IPC$",
					refCount:   1,
				},
				shares: shares,
			}

			tc = &ft.treeConn
			c.treeMapByName["\\IPC$"] = ft
			c.treeMapById[tc.treeId] = ft
			log.Debugf("new ipc tree %d", tc.treeId)
		}

		err = c.sendPacket(rsp, tc, nil)
	} else {
		parts := strings.Split(r.Path(), "\\")
		if len(parts) < 1 {
			rsp.Status = uint32(STATUS_BAD_NETWORK_NAME)
			return c.sendPacket(rsp, nil, nil)
		}
		path := parts[len(parts)-1]

		c.serverCtx.lock.Lock()
		fs, ok := c.serverCtx.shares[strings.ToUpper(path)]
		if !ok {
			fs, ok = c.serverCtx.shares[strings.ToUpper(path)+"$"]
		}
		if !ok {
			shares := maps.Keys(c.serverCtx.shares)
			c.serverCtx.lock.Unlock()
			log.Debugf("shares: %v", shares)
			rsp.Status = uint32(STATUS_BAD_NETWORK_NAME)
			return c.sendPacket(rsp, nil, nil)
		}
		c.serverCtx.lock.Unlock()

		rsp.ShareType = SMB2_SHARE_TYPE_DISK
		rsp.MaximalAccess = SYNCHRONIZE | WRITE_OWNER | WRITE_DAC | READ_CONTROL | DELETE |
			FILE_READ_ATTRIBUTES | FILE_EXECUTE | FILE_READ_EA | FILE_READ_DATA

		var tc *treeConn
		if t, ok := c.treeMapByName[path]; ok {
			rsp.TreeId = t.getTree().treeId
			tc = t.getTree()
			tc.refCount++
		} else {
			maxIOWrites, maxIoReads := DEFAULT_IOPS, DEFAULT_IOPS
			if c.serverCtx.maxIOReads > 0 {
				maxIoReads = c.serverCtx.maxIOReads
			}
			if c.serverCtx.maxIOWrites > 0 {
				maxIOWrites = c.serverCtx.maxIOWrites
			}
			ft := &fileTree{
				treeConn: treeConn{
					session:    c.session,
					treeId:     randint32(),
					shareFlags: 0,
					path:       path,
					refCount:   1,
				},
				fs:         fs,
				openFiles:  make(map[uint64]bool),
				ioReadSem:  make(chan struct{}, maxIoReads),
				ioWriteSem: make(chan struct{}, maxIOWrites),
			}

			tc = &ft.treeConn
			c.treeMapByName[path] = ft
			c.treeMapById[tc.treeId] = ft
			log.Debugf("new file tree %d", ft.treeId)
		}

		err = c.sendPacket(rsp, tc, nil)
	}

	return err
}

func (c *conn) treeDisconnect(pkt []byte) error {
	log.Debugf("TreeDisconnect")

	p := PacketCodec(pkt)

	tc, ok := c.treeMapById[p.TreeId()]
	if !ok {
		log.Warnf(fmt.Sprintf("tree doesn't exist: %d", p.TreeId()))
	}

	var tree *treeConn = nil
	if tc != nil {
		tree = tc.getTree()
		tree.refCount--
		if tree.refCount == 0 {
			if err := c.closeTreeHandles(tree); err != nil {
				rsp := new(ErrorResponse)
				PrepareResponse(rsp.Header(), pkt, uint32(statusFromError(err)))
				return c.sendPacket(rsp, tree, nil)
			}
			log.Debugf("Deleting tree %s", tree.path)
			delete(c.treeMapByName, tree.path)
			delete(c.treeMapById, tree.treeId)
		}
	}

	rsp := new(TreeDisconnectResponse)
	rsp.CreditRequestResponse = p.CreditRequest()
	rsp.MessageId = p.MessageId()
	rsp.Flags = 1

	return c.sendPacket(rsp, tree, nil)
}

func (n *ServerNegotiator) negotiate(conn *conn, pkt []byte) error {
	p := PacketCodec(pkt)
	if p.IsSmb1() {
		n.SpecifiedDialect = SMB2
		rsp, _ := n.makeResponse(conn)
		return conn.sendPacket(rsp, nil, nil)
	}

	res, err := accept(SMB2_NEGOTIATE, pkt)
	if err != nil {
		return err
	}

	r := NegotiateRequestDecoder(res)
	if r.IsInvalid() {
		return &InvalidResponseError{"broken negotiate response format"}
	}

	n.SpecifiedDialect = uint16(SMB2)
	for _, d := range r.Dialects() {
		if d > n.SpecifiedDialect && d <= SMB311 {
			n.SpecifiedDialect = d
		}
	}

	if n.SpecifiedDialect == SMB2 {
		n.SpecifiedDialect = SMB210
	}

	if n.SpecifiedDialect == UnknownSMB {
		return &InvalidResponseError{"unexpected dialect returned"}
	}

	conn.requireSigning = n.RequireMessageSigning || r.SecurityMode()&SMB2_NEGOTIATE_SIGNING_REQUIRED != 0
	conn.capabilities = serverCapabilities & r.Capabilities()
	conn.dialect = n.SpecifiedDialect
	conn.maxTransactSize = serverMaxTransactSize
	conn.maxReadSize = serverMaxReadSize
	conn.maxWriteSize = serverMaxWriteSize
	conn.sequenceWindow = 1

	// conn.gssNegotiateToken = r.SecurityBuffer()
	// conn.clientGuid = n.ClientGuid
	// copy(conn.serverGuid[:], r.ServerGuid())

	auth := conn.serverCtx.authenticator
	if a, ok := auth.(*NTLMAuthenticator); ok {
		copy := *a
		copy.ntlm = nil
		copy.seqNum = 0
		auth = &copy
	}
	conn.spnego = newSpnegoServer([]Authenticator{auth})
	outputToken, _ := conn.spnego.initSecContext()

	if conn.dialect != SMB311 {
		rsp, _ := n.makeResponse(conn)
		PrepareResponse(&rsp.PacketHeader, pkt, uint32(0))
		rsp.SecurityBuffer = outputToken
		return conn.sendPacket(rsp, nil, nil)
	}

	// handle context for SMB311
	list := r.NegotiateContextList()
	for count := r.NegotiateContextCount(); count > 0; count-- {
		ctx := NegotiateContextDecoder(list)
		if ctx.IsInvalid() {
			return &InvalidResponseError{"broken negotiate context format"}
		}

		switch ctx.ContextType() {
		case SMB2_PREAUTH_INTEGRITY_CAPABILITIES:
			d := HashContextDataDecoder(ctx.Data())
			if d.IsInvalid() {
				return &InvalidResponseError{"broken hash context data format"}
			}

			algs := d.HashAlgorithms()

			if len(algs) != 1 {
				return &InvalidResponseError{"multiple hash algorithms"}
			}

			conn.preauthIntegrityHashId = algs[0]
			conn.calcPreauthHash(pkt)
		case SMB2_ENCRYPTION_CAPABILITIES:
			d := CipherContextDataDecoder(ctx.Data())
			if d.IsInvalid() {
				return &InvalidResponseError{"broken cipher context data format"}
			}

			ciphs := d.Ciphers()
			for _, ciph := range ciphs {
				if ciph == AES128CCM || ciph == AES128GCM {
					conn.cipherId = ciph
					break
				}
			}

			switch conn.cipherId {
			case AES128CCM:
			case AES128GCM:
			default:
				return &InvalidResponseError{"unknown cipher algorithm"}
			}
		case SMB3_POSIX_EXTENSIONS_AVAILABLE:
			if conn.serverCtx.enableSMB3PosixExtensions && ctx.IsSMB3Posix() {
				conn.posixExtensions = true
			}
		default:
			// skip unsupported context
		}

		off := ctx.Next()

		if len(list) < off {
			list = nil
		} else {
			list = list[off:]
		}
	}

	rsp, _ := n.makeResponse(conn)
	PrepareResponse(&rsp.PacketHeader, pkt, uint32(0))
	rsp.SecurityBuffer = outputToken
	return conn.sendPacket(rsp, nil, nil)
}

func (n *ServerNegotiator) makeResponse(conn *conn) (*NegotiateResponse, error) {
	rsp := new(NegotiateResponse)

	if n.RequireMessageSigning {
		rsp.SecurityMode = SMB2_NEGOTIATE_SIGNING_REQUIRED
	} else {
		rsp.SecurityMode = SMB2_NEGOTIATE_SIGNING_ENABLED
	}
	rsp.Flags = 1

	rsp.Capabilities = serverCapabilities
	rsp.MaxTransactSize = serverMaxTransactSize
	rsp.MaxReadSize = serverMaxReadSize
	rsp.MaxWriteSize = serverMaxWriteSize
	rsp.SystemTime = NsecToFiletime(time.Now().UnixNano())
	rsp.ServerStartTime = &Filetime{}
	rsp.ServerGuid = conn.serverCtx.serverGuid

	if n.SpecifiedDialect != UnknownSMB {
		rsp.DialectRevision = n.SpecifiedDialect

		switch n.SpecifiedDialect {
		case SMB2:
		case SMB202:
		case SMB210:
		case SMB300:
		case SMB302:
		case SMB311:
			hc := &HashContext{
				HashAlgorithms: []uint16{conn.hashId},
				HashSalt:       make([]byte, 32),
			}
			if _, err := rand.Read(hc.HashSalt); err != nil {
				return nil, &InternalError{err.Error()}
			}

			cc := &CipherContext{
				Ciphers: []uint16{conn.cipherId},
			}

			rsp.Contexts = append(rsp.Contexts, hc, cc)
			if conn.posixExtensions {
				rsp.Contexts = append(rsp.Contexts, &PosixContext{})
			}
		default:
			return nil, &InternalError{"unsupported dialect specified"}
		}
	} else {
		rsp.DialectRevision = defaultDerverDialect

		hc := &HashContext{
			HashAlgorithms: clientHashAlgorithms,
			HashSalt:       make([]byte, 32),
		}
		if _, err := rand.Read(hc.HashSalt); err != nil {
			return nil, &InternalError{err.Error()}
		}

		cc := &CipherContext{
			Ciphers: clientCiphers,
		}

		rsp.Contexts = append(rsp.Contexts, hc, cc)
	}

	return rsp, nil
}

func randint32() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return uint32(binary.LittleEndian.Uint32(b[:]))
}

func randint64() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return uint64(binary.LittleEndian.Uint64(b[:]))
}

func (c *conn) calcPreauthHash(pkt []byte) {
	switch c.dialect {
	case SMB311:
		switch c.preauthIntegrityHashId {
		case SHA512:
			h := sha512.New()
			h.Write(c.preauthIntegrityHashValue[:])
			h.Write(pkt)
			h.Sum(c.preauthIntegrityHashValue[:0])
		}
	}
}

func (c *conn) sessionServerSetup(pkt []byte) error {
	log.Debugf("sessionServerSetup")

	p := PacketCodec(pkt)

	res, err := accept(SMB2_SESSION_SETUP, pkt)
	if err != nil {
		log.Debugf("sessionServerSetup: %v", err)
		return err
	}

	c.calcPreauthHash(pkt)

	r := SessionSetupRequestDecoder(res)
	if r.IsInvalid() {
		log.Debugf("sessionServerSetup invalid")
		return &InvalidRequestError{"broken session setup request format"}
	}

	/*if c.requireSigning && r.SecurityMode() != SMB2_NEGOTIATE_SIGNING_REQUIRED {
		return &InvalidRequestError{"request security mode doesn't match connection requirement"}
	}*/

	outputToken, err := c.spnego.challenge(r.SecurityBuffer())
	if err != nil {
		log.Debugf("sessionServerSetup challenge: %v", err)
		return &InvalidRequestError{err.Error()}
	}

	rsp := &SessionSetupResponse{
		SessionFlags:   0,
		SecurityBuffer: outputToken,
	}

	sessionId := randint64()

	rsp.Flags = 1
	rsp.CreditRequestResponse = p.CreditRequest()
	rsp.CreditCharge = 0 //c.CreditCharge()
	rsp.MessageId = p.MessageId()
	rsp.Status = uint32(STATUS_MORE_PROCESSING_REQUIRED)
	rsp.SessionId = sessionId

	c.serverState = STATE_SESSION_SETUP_CHALLENGE

	return c.sendPacket(rsp, nil, nil)
}

func (c *conn) sessionServerSetupChallenge(pkt []byte) error {
	log.Debugf("sessionServerSetupChallenge")

	p := PacketCodec(pkt)

	res, err := accept(SMB2_SESSION_SETUP, pkt)
	if err != nil {
		return err
	}

	r := SessionSetupRequestDecoder(res)
	if r.IsInvalid() {
		return &InvalidResponseError{"broken session setup request format"}
	}

	c.calcPreauthHash(pkt)

	outputToken, user, err := c.spnego.authenticate(r.SecurityBuffer())
	if err != nil {
		rsp := new(ErrorResponse)
		PrepareResponse(&rsp.PacketHeader, pkt, uint32(STATUS_ACCESS_DENIED))
		c.sendPacket(rsp, nil, nil)
		return &InvalidRequestError{err.Error()}
	}

	log.Debugf("auth user: %s", user)
	flags := uint16(0)
	if c.serverCtx.allowGuest {
		flags = SMB2_SESSION_FLAG_IS_GUEST
	}

	sessionId := p.SessionId()
	s := &session{
		conn:           c,
		treeConnTables: make(map[uint32]*treeConn),
		sessionFlags:   flags,
		sessionId:      sessionId,
	}

	rsp := &SessionSetupResponse{
		SessionFlags:   s.sessionFlags,
		SecurityBuffer: outputToken,
	}

	rsp.Flags = 1
	rsp.CreditRequestResponse = p.CreditRequest()
	rsp.CreditCharge = 0 //c.CreditCharge()
	rsp.MessageId = p.MessageId()
	rsp.SessionId = sessionId
	rsp.SecurityBuffer = outputToken

	if s.sessionFlags&(SMB2_SESSION_FLAG_IS_GUEST|SMB2_SESSION_FLAG_IS_NULL) == 0 {
		sessionKey := c.spnego.sessionKey()
		switch c.dialect {
		case SMB202, SMB210:
			s.signer = hmac.New(sha256.New, sessionKey)
			s.verifier = hmac.New(sha256.New, sessionKey)
		case SMB300, SMB302:
			signingKey := kdf(sessionKey, []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"))
			ciph, err := aes.NewCipher(signingKey)
			if err != nil {
				return &InternalError{err.Error()}
			}
			s.signer = cmac.New(ciph)
			s.verifier = cmac.New(ciph)

			// s.applicationKey = kdf(sessionKey, []byte("SMB2APP\x00"), []byte("SmbRpc\x00"))

			encryptionKey := kdf(sessionKey, []byte("SMB2AESCCM\x00"), []byte("ServerIn \x00"))
			decryptionKey := kdf(sessionKey, []byte("SMB2AESCCM\x00"), []byte("ServerOut\x00"))

			ciph, err = aes.NewCipher(encryptionKey)
			if err != nil {
				return &InternalError{err.Error()}
			}
			s.encrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
			if err != nil {
				return &InternalError{err.Error()}
			}

			ciph, err = aes.NewCipher(decryptionKey)
			if err != nil {
				return &InternalError{err.Error()}
			}
			s.decrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
			if err != nil {
				return &InternalError{err.Error()}
			}
		case SMB311:
			s.preauthIntegrityHashValue = c.preauthIntegrityHashValue
			signingKey := kdf(sessionKey, []byte("SMBSigningKey\x00"), s.preauthIntegrityHashValue[:])
			ciph, err := aes.NewCipher(signingKey)
			if err != nil {
				return &InternalError{err.Error()}
			}
			s.signer = cmac.New(ciph)
			s.verifier = cmac.New(ciph)

			encryptionKey := kdf(sessionKey, []byte("SMBC2CCipherKey\x00"), s.preauthIntegrityHashValue[:])
			decryptionKey := kdf(sessionKey, []byte("SMBS2SCipherKey\x00"), s.preauthIntegrityHashValue[:])

			switch c.cipherId {
			case AES128CCM:
				ciph, err := aes.NewCipher(encryptionKey)
				if err != nil {
					return &InternalError{err.Error()}
				}
				s.encrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
				if err != nil {
					return &InternalError{err.Error()}
				}

				ciph, err = aes.NewCipher(decryptionKey)
				if err != nil {
					return &InternalError{err.Error()}
				}
				s.decrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
				if err != nil {
					return &InternalError{err.Error()}
				}
			case AES128GCM:
				ciph, err := aes.NewCipher(encryptionKey)
				if err != nil {
					return &InternalError{err.Error()}
				}
				s.encrypter, err = cipher.NewGCMWithNonceSize(ciph, 12)
				if err != nil {
					return &InternalError{err.Error()}
				}

				ciph, err = aes.NewCipher(decryptionKey)
				if err != nil {
					return &InternalError{err.Error()}
				}
				s.decrypter, err = cipher.NewGCMWithNonceSize(ciph, 12)
				if err != nil {
					return &InternalError{err.Error()}
				}
			}
		}
	}

	// We set session before sending packet just for setting hdr.SessionId.
	// But, we should not permit access from receiver until the session information is completed.
	c.registerSession(s)

	c.serverState = STATE_SESSION_ACTIVE
	// Keys and session fields are complete before publishing to the receiver.
	// Publish before the response reaches the client, otherwise its first request
	// can race the receiver's signing gate.
	c.enableSession()
	return c.sendPacket(rsp, nil, nil)
}

func (d *Server) addOpen(open *Open) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.opens[open.fileId] = open
	if open.isDurable {
		d.opensByGuid[open.createGuid] = open
	}
}

func (d *Server) getOpen(fileId uint64) *Open {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.opens[fileId]
}

func (d *Server) deleteOpen(fileId uint64) {
	d.lock.Lock()
	defer d.lock.Unlock()
	defer d.broadcastLockWaiters()
	if open, ok := d.opens[fileId]; ok {
		delete(d.opens, fileId)
		if open.isDurable {
			delete(d.opensByGuid, open.createGuid)
		}
	}
}

func (d *Server) setDeletePending(node uint64, pending bool) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if pending {
		d.deletePending[node] = true
	} else {
		delete(d.deletePending, node)
	}
}

func (d *Server) isDeletePending(node uint64) bool {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.deletePending[node]
}

func (d *Server) closeOpen(open *Open) bool {
	d.lock.Lock()
	defer d.lock.Unlock()
	defer d.broadcastLockWaiters()

	if open.deleteOnClose {
		d.deletePending[open.durableFileId] = true
	}

	delete(d.opens, open.fileId)
	if open.isDurable {
		delete(d.opensByGuid, open.createGuid)
	}

	if !d.deletePending[open.durableFileId] {
		return false
	}

	for _, other := range d.opens {
		if other.durableFileId == open.durableFileId {
			return false
		}
	}

	delete(d.deletePending, open.durableFileId)
	return true
}

// broadcastLockWaiters tolerates Server values constructed directly by tests
// and embedders instead of through NewServer.
func (d *Server) broadcastLockWaiters() {
	if d.lockCond != nil {
		d.lockCond.Broadcast()
	}
}

func (d *Server) BreakNode(node uint64) {
	opens := d.opensForNode(node)
	for _, open := range opens {
		if err := d.breakOpen(open); err != nil {
			log.WithError(err).Debugf("failed to break cache for node=%d handle=%d", node, open.fileId)
		}
	}
}

func (d *Server) opensForNode(node uint64) []*Open {
	d.lock.Lock()
	defer d.lock.Unlock()

	opens := make([]*Open, 0)
	for _, open := range d.opens {
		if open.durableFileId == node {
			opens = append(opens, open)
		}
	}
	return opens
}

func (d *Server) breakOpen(open *Open) error {
	if open == nil || open.session == nil || open.session.conn == nil {
		return nil
	}

	switch open.oplockLevel {
	case SMB2_OPLOCK_LEVEL_NONE:
		return nil
	case SMB2_OPLOCK_LEVEL_LEASE:
		return d.sendLeaseBreak(open)
	default:
		return d.sendOplockBreak(open)
	}
}

func (d *Server) sendOplockBreak(open *Open) error {
	if open.oplockState == LOCKSTATE_BREAKING {
		return nil
	}

	fileID := &FileId{}
	fileID.SetHandleId(open.fileId)
	fileID.SetNodeId(open.durableFileId)

	rsp := &OplockBreakNotification{
		OplockLevel: SMB2_OPLOCK_LEVEL_NONE,
		FileId:      fileID,
	}
	rsp.MessageId = ^uint64(0)
	rsp.Flags = SMB2_FLAGS_SERVER_TO_REDIR
	rsp.CreditRequestResponse = 1

	if open.oplockLevel == SMB2_OPLOCK_LEVEL_II {
		open.oplockLevel = SMB2_OPLOCK_LEVEL_NONE
		open.oplockState = LOCKSTATE_NONE
	} else {
		open.oplockState = LOCKSTATE_BREAKING
	}

	log.Debugf("sending oplock break node=%d handle=%d level=%d", open.durableFileId, open.fileId, open.oplockLevel)
	return open.session.conn.sendPacket(rsp, nil, nil)
}

func (d *Server) sendLeaseBreak(open *Open) error {
	if open.lease == nil || open.lease.Breaking {
		return nil
	}

	open.lease.Epoch++
	open.lease.Breaking = true
	open.lease.BreakToState = SMB2_LEASE_NONE
	open.oplockState = LOCKSTATE_BREAKING

	rsp := &LeaseBreakNotification{
		NewEpoch:          open.lease.Epoch,
		Flags:             SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED,
		LeaseKey:          open.lease.LeaseKey,
		CurrentLeaseState: open.lease.LeaseState,
		NewLeaseState:     SMB2_LEASE_NONE,
	}
	rsp.MessageId = ^uint64(0)
	rsp.Flags = SMB2_FLAGS_SERVER_TO_REDIR
	rsp.CreditRequestResponse = 1

	log.Debugf("sending lease break node=%d handle=%d lease_state=%d", open.durableFileId, open.fileId, open.lease.LeaseState)
	return open.session.conn.sendPacket(rsp, nil, nil)
}
