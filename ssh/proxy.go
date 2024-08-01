// This file is implemented with reference to tg123/sshpiper.
// Ref: https://github.com/tg123/sshpiper/blob/master/vendor/golang.org/x/crypto/ssh/sshpiper.go
// Thanks to @tg123

package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
)

type userFile string

var (
	userAuthorizedKeysFile userFile = "authorized_keys"
	userPrivateKeyFile     userFile = "id_rsa"
)

type AuthType int

type ProxyConfig struct {
	Config
	ServerConfig    *ServerConfig
	ClientConfig    *ClientConfig
	DestinationPort string
	// Specify upstream host by SSH username
	FindUpstreamHook func(username string) (string, error)
	// Fetch authorized_keys to confirm registration of the client's public key.
	FetchAuthorizedKeysHook func(username string, host string) ([]byte, error)
	// Fetch the private key used when sshr performs public key authentication as a client user
	// to the upstream host
	FetchPrivateKeyHook func(username string) ([]byte, error)
	// When using only the master key when sending requests to the upstream server, set A to true.
	UseMasterKey  bool
	MasterKeyPath string
}

type ProxyConn struct {
	UpUser          string
	DownUser        string
	DestinationHost string
	Upstream        *connection
	Downstream      *connection
}

func (p *ProxyConn) handleAuthMsg(msg *userAuthRequestMsg, proxyConf *ProxyConfig) (*userAuthRequestMsg, error) {
	switch msg.Method {
	case "publickey":
		if proxyConf.FetchAuthorizedKeysHook == nil {
			proxyConf.FetchAuthorizedKeysHook = fetchAuthorizedKeysFromHomeDir
		}

		downStreamPublicKey, isQuery, sig, algo, err := parsePublicKeyMsg(msg)
		if err != nil {
			break
		}

		if isQuery {
			if err := p.sendOKMsg(downStreamPublicKey); err != nil {
				return nil, err
			}
			return nil, nil
		}

		authKeys, err := proxyConf.FetchAuthorizedKeysHook(p.DownUser, p.DestinationHost)
		if err != nil {
			return noneAuthMsg(p.UpUser), nil
		}

		ok, err := checkPublicKeyRegistration(authKeys, downStreamPublicKey)
		if err != nil || !ok {
			return noneAuthMsg(p.UpUser), nil
		}

		ok, err = p.VerifySignature(msg, downStreamPublicKey, algo, sig)
		if err != nil || !ok {
			break
		}

		privateBytes, err := fetchPrivateKey(proxyConf, p.UpUser)
		if err != nil {
			break
		}

		signer, err := ParsePrivateKey(privateBytes)
		if err != nil || signer == nil {
			break
		}

		authMethod := PublicKeys(signer)
		f, ok := authMethod.(publicKeyCallback)
		if !ok {
			break
		}

		signers, err := f()
		if err != nil || len(signers) == 0 {
			break
		}

		for _, signer := range signers {
			msg, err = p.signAgain(p.UpUser, msg, signer)
			if err == nil {
				return msg, nil
			}
		}

	case "password":
		// In the case of password authentication,
		// since authentication is left up to the upstream server,
		// it suffices to flow the packet as it is.
		return msg, nil

	default:
		return msg, nil
	}

	err := p.SendFailureMsg(msg.Method)
	return nil, err
}

func checkPublicKeyRegistration(authKeys []byte, publicKey PublicKey) (bool, error) {
	publicKeyData := publicKey.Marshal()

	var err error
	var authorizedPublicKey PublicKey
	for len(authKeys) > 0 {
		authorizedPublicKey, _, _, authKeys, err = ParseAuthorizedKey(authKeys)
		if err != nil {
			return false, err
		}

		if bytes.Equal(authorizedPublicKey.Marshal(), publicKeyData) {
			return true, nil
		}
	}
	return false, nil
}

func fetchAuthorizedKeysFromHomeDir(username string, host string) ([]byte, error) {
	var authKeys []byte
	authKeys, err := userAuthorizedKeysFile.read(username)
	if err != nil {
		return nil, err
	}
	return authKeys, nil
}

func fetchPrivateKey(proxyConf *ProxyConfig, username string) ([]byte, error) {
	var privateBytes []byte
	var err error
	if proxyConf.UseMasterKey {
		privateBytes, err = ioutil.ReadFile(proxyConf.MasterKeyPath)
		if err != nil {
			return nil, err
		}
	} else if proxyConf.FetchPrivateKeyHook == nil {
		privateBytes, err = fetchPrivateKeyFromHomeDir(username)
		if err != nil {
			return nil, err
		}
	} else {
		privateBytes, err = proxyConf.FetchPrivateKeyHook(username)
		if err != nil {
			return nil, err
		}
	}
	return privateBytes, nil
}

func fetchPrivateKeyFromHomeDir(username string) ([]byte, error) {
	var privateBytes []byte
	privateBytes, err := userPrivateKeyFile.read(username)
	if err != nil {
		return nil, err
	}
	return privateBytes, nil
}

func (file userFile) checkPermission(user string) error {
	filename := userSpecFile(user, string(file))
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%v's perm is too open", filename)
	}

	return nil
}

func userSpecFile(username, file string) string {
	return path.Join("/home", username, "/.ssh", file)
}

func (p *ProxyConn) sendOKMsg(key PublicKey) error {
	okMsg := userAuthPubKeyOkMsg{
		Algo:   key.Type(),
		PubKey: key.Marshal(),
	}

	return p.Downstream.transport.writePacket(Marshal(&okMsg))
}

func (p *ProxyConn) SendFailureMsg(method string) error {
	var failureMsg userAuthFailureMsg
	failureMsg.Methods = append(failureMsg.Methods, method)

	return p.Downstream.transport.writePacket(Marshal(&failureMsg))
}

func (file userFile) read(username string) ([]byte, error) {
	return ioutil.ReadFile(userSpecFile(username, string(file)))
}

func (p *ProxyConn) VerifySignature(msg *userAuthRequestMsg, publicKey PublicKey, algo string, sig *Signature) (bool, error) {
	if !contains(supportedPubKeyAuthAlgos, sig.Format) {
		return false, fmt.Errorf("ssh: algorithm %q not accepted", sig.Format)
	}
	signedData := buildDataSignedForAuth(p.Downstream.transport.getSessionID(), *msg, algo, publicKey.Marshal())

	if err := publicKey.Verify(signedData, sig); err != nil {
		return false, nil
	}

	return true, nil
}

func (p *ProxyConn) signAgain(user string, msg *userAuthRequestMsg, signer Signer) (*userAuthRequestMsg, error) {
	rand := p.Upstream.transport.config.Rand
	sessionID := p.Upstream.transport.getSessionID()
	upStreamPublicKey := signer.PublicKey()
	upStreamPublicKeyData := upStreamPublicKey.Marshal()

	sign, err := signer.Sign(rand, buildDataSignedForAuth(sessionID, userAuthRequestMsg{
		User:    user,
		Service: serviceSSH,
		Method:  "publickey",
	}, upStreamPublicKey.Type(), upStreamPublicKeyData))
	if err != nil {
		return nil, err
	}

	// manually wrap the serialized signature in a string
	s := Marshal(sign)
	sig := make([]byte, stringLength(len(s)))
	marshalString(sig, s)

	publicKeyMsg := &publickeyAuthMsg{
		User:     user,
		Service:  serviceSSH,
		Method:   "publickey",
		HasSig:   true,
		Algoname: upStreamPublicKey.Type(),
		PubKey:   upStreamPublicKeyData,
		Sig:      sig,
	}

	Unmarshal(Marshal(publicKeyMsg), msg)

	return msg, nil
}

func (p *ProxyConn) Wait() error {
	c := make(chan error, 1)

	go func() {
		c <- piping(p.Upstream.transport, p.Downstream.transport)
	}()

	go func() {
		c <- piping(p.Downstream.transport, p.Upstream.transport)
	}()

	defer p.Close()
	return <-c
}

func (p *ProxyConn) Close() {
	if p.Upstream != nil {
		if p.Upstream.transport != nil {
			p.Upstream.transport.Close()
		}

		p.Upstream.Close()

	}
	if p.Downstream != nil {
		if p.Downstream.transport != nil {
			p.Downstream.transport.Close()
		}

		p.Downstream.Close()
	}
}

func (p *ProxyConn) checkBridgeAuthWithNoBanner(packet []byte) (bool, error) {
	err := p.Upstream.transport.writePacket(packet)
	if err != nil {
		return false, err
	}

	for {
		packet, err := p.Upstream.transport.readPacket()
		if err != nil {
			return false, err
		}

		msgType := packet[0]

		if err = p.Downstream.transport.writePacket(packet); err != nil {
			return false, err
		}

		switch msgType {
		case msgUserAuthSuccess:
			return true, nil
		case msgUserAuthBanner:
			continue
		case msgUserAuthFailure:
		default:
		}

		return false, nil
	}
}

func (p *ProxyConn) AuthenticateProxyConn(initUserAuthMsg *userAuthRequestMsg, proxyConf *ProxyConfig) error {
	err := p.Upstream.sendAuthReq()
	if err != nil {
		return err
	}

	userAuthMsg := initUserAuthMsg
	for {
		userAuthMsg, err = p.handleAuthMsg(userAuthMsg, proxyConf)
		if err != nil {
			fmt.Println(err)
		}

		if userAuthMsg != nil {
			isSuccess, err := p.checkBridgeAuthWithNoBanner(Marshal(userAuthMsg))
			if err != nil {
				return err
			}
			if isSuccess {
				return nil
			}
		}

		var packet []byte

		for {
			// Read next msg after a failure
			if packet, err = p.Downstream.transport.readPacket(); err != nil {
				return err
			}

			if packet[0] == msgUserAuthRequest {
				break
			}

			return errors.New("auth request msg can be acceptable")
		}

		var userAuthReq userAuthRequestMsg

		if err = Unmarshal(packet, &userAuthReq); err != nil {
			return err
		}

		userAuthMsg = &userAuthReq
	}
}

func parsePublicKeyMsg(userAuthReq *userAuthRequestMsg) (PublicKey, bool, *Signature, string, error) {
	if userAuthReq.Method != "publickey" {
		return nil, false, nil, "", fmt.Errorf("not a publickey auth msg")
	}

	payload := userAuthReq.Payload
	if len(payload) < 1 {
		return nil, false, nil, "", parseError(msgUserAuthRequest)
	}
	isQuery := payload[0] == 0
	payload = payload[1:]
	algoBytes, payload, ok := parseString(payload)
	if !ok {
		return nil, false, nil, "", parseError(msgUserAuthRequest)
	}
	algo := string(algoBytes)
	if !contains(supportedPubKeyAuthAlgos, underlyingAlgo(algo)) {
		return nil, false, nil, "", fmt.Errorf("ssh: algorithm %q not accepted", algo)
	}

	pubKeyData, payload, ok := parseString(payload)
	if !ok {
		return nil, false, nil, "", parseError(msgUserAuthRequest)
	}

	publicKey, err := ParsePublicKey(pubKeyData)
	if err != nil {
		return nil, false, nil, "", err
	}

	var sig *Signature
	if !isQuery {
		sig, payload, ok = parseSignature(payload)
		if !ok || len(payload) > 0 {
			return nil, false, nil, "", parseError(msgUserAuthRequest)
		}
	}

	return publicKey, isQuery, sig, algo, nil
}

func piping(dst, src packetConn) error {
	for {
		p, err := src.readPacket()
		if err != nil {
			return err
		}

		if err := dst.writePacket(p); err != nil {
			return err
		}
	}
}

func noneAuthMsg(user string) *userAuthRequestMsg {
	return &userAuthRequestMsg{
		User:    user,
		Service: serviceSSH,
		Method:  "none",
	}
}

func NewDownstreamConn(c net.Conn, config *ServerConfig) (*connection, error) {
	fullConf := *config
	fullConf.SetDefaults()

	if len(fullConf.PublicKeyAuthAlgorithms) == 0 {
		fullConf.PublicKeyAuthAlgorithms = supportedPubKeyAuthAlgos
	} else {
		for _, algo := range fullConf.PublicKeyAuthAlgorithms {
			if !contains(supportedPubKeyAuthAlgos, algo) {
				c.Close()
				return nil, fmt.Errorf("ssh: unsupported public key authentication algorithm %s", algo)
			}
		}
	}

	conn := &connection{
		sshConn: sshConn{conn: c},
	}

	_, err := conn.serverHandshakeWithNoAuth(&fullConf)
	if err != nil {
		c.Close()
		return nil, err
	}

	return conn, nil
}

func NewUpstreamConn(c net.Conn, config *ClientConfig) (*connection, error) {
	fullConf := *config
	fullConf.SetDefaults()

	conn := &connection{
		sshConn: sshConn{conn: c},
	}

	if err := conn.clientHandshakeWithNoAuth(c.RemoteAddr().String(), &fullConf); err != nil {
		c.Close()
		return nil, fmt.Errorf("ssh: handshake failed: %v", err)
	}

	return conn, nil
}

func (c *connection) sendAuthReq() error {
	// initiate user auth session
	if err := c.transport.writePacket(Marshal(&serviceRequestMsg{serviceUserAuth})); err != nil {
		return err
	}

	packet, err := c.transport.readPacket()
	if err != nil {
		return err
	}

	// The server may choose to send a SSH_MSG_EXT_INFO at this point (if we
	// advertised willingness to receive one, which we always do) or not. See
	// RFC 8308, Section 2.4.
	extensions := make(map[string][]byte)
	if len(packet) > 0 && packet[0] == msgExtInfo {
		var extInfo extInfoMsg
		if err := Unmarshal(packet, &extInfo); err != nil {
			return err
		}
		payload := extInfo.Payload
		for i := uint32(0); i < extInfo.NumExtensions; i++ {
			name, rest, ok := parseString(payload)
			if !ok {
				return parseError(msgExtInfo)
			}
			value, rest, ok := parseString(rest)
			if !ok {
				return parseError(msgExtInfo)
			}
			extensions[string(name)] = value
			payload = rest
		}
		packet, err = c.transport.readPacket()
		if err != nil {
			return err
		}
	}

	var serviceAccept serviceAcceptMsg
	return Unmarshal(packet, &serviceAccept)
}

func (c *connection) GetAuthRequestMsg() (*userAuthRequestMsg, error) {
	var userAuthReq userAuthRequestMsg

	if packet, err := c.transport.readPacket(); err != nil {
		return nil, err
	} else if err = Unmarshal(packet, &userAuthReq); err != nil {
		return nil, err
	}

	if userAuthReq.Service != serviceSSH {
		return nil, errors.New("ssh: client attempted to negotiate for unknown service: " + userAuthReq.Service)
	}

	// User is combined by {username}#{upstreamHost}
	c.user = userAuthReq.User
	return &userAuthReq, nil
}

func (c *connection) clientHandshakeWithNoAuth(dialAddress string, config *ClientConfig) error {
	c.clientVersion = []byte(packageVersion)
	if config.ClientVersion != "" {
		c.clientVersion = []byte(config.ClientVersion)
	}

	var err error
	c.serverVersion, err = exchangeVersions(c.sshConn.conn, c.clientVersion)
	if err != nil {
		return err
	}

	c.transport = newClientTransport(
		newTransport(c.sshConn.conn, config.Rand, true /* is client */),
		c.clientVersion, c.serverVersion, config, dialAddress, c.sshConn.RemoteAddr())

	if err := c.transport.waitSession(); err != nil {
		return err
	}

	c.sessionID = c.transport.getSessionID()
	return nil
}

func (c *connection) serverHandshakeWithNoAuth(config *ServerConfig) (*Permissions, error) {
	if len(config.hostKeys) == 0 {
		return nil, errors.New("ssh: server has no host keys")
	}

	var err error
	if config.ServerVersion != "" {
		c.serverVersion = []byte(config.ServerVersion)
	} else {
		c.serverVersion = []byte("SSH-2.0-sshr")
	}
	c.clientVersion, err = exchangeVersions(c.sshConn.conn, c.serverVersion)
	if err != nil {
		return nil, err
	}

	tr := newTransport(c.sshConn.conn, config.Rand, false /* not client */)
	c.transport = newServerTransport(tr, c.clientVersion, c.serverVersion, config)

	if err := c.transport.waitSession(); err != nil {
		return nil, err

	}
	c.sessionID = c.transport.getSessionID()

	var packet []byte
	if packet, err = c.transport.readPacket(); err != nil {
		return nil, err
	}

	var serviceRequest serviceRequestMsg
	if err = Unmarshal(packet, &serviceRequest); err != nil {
		return nil, err
	}
	if serviceRequest.Service != serviceUserAuth {
		return nil, errors.New("ssh: requested service '" + serviceRequest.Service + "' before authenticating")
	}
	serviceAccept := serviceAcceptMsg{
		Service: serviceUserAuth,
	}
	if err := c.transport.writePacket(Marshal(&serviceAccept)); err != nil {
		return nil, err
	}

	return nil, nil
}
