package ssh

import (
	"errors"
	"fmt"
	"net"
)

type SSHPiper struct {
	DownstreamConfig ServerConfig

	AdditionalChallenge func(conn ConnMetadata, client KeyboardInteractiveChallenge) (bool, error)
	FindUpstream        func(conn ConnMetadata) (net.Conn, *ClientConfig, error)
	MapPublicKey        func(conn ConnMetadata, key PublicKey) (Signer, error)
}

type upstream struct{ *connection }
type downstream struct{ *connection }

type pipedConn struct {
	upstream   *upstream
	downstream *downstream

	processAuthMsg func(msg *userAuthRequestMsg) (*userAuthRequestMsg, error)
}

func (piper *SSHPiper) Serve(conn net.Conn) error {

	d, err := newDownstream(conn, &piper.DownstreamConfig)
	if err != nil {
		return err
	}

	defer d.Close()

	userAuthReq, err := d.nextAuthMsg()
	if err != nil {
		return err
	}

	d.user = userAuthReq.User

	// need additional challenge
	if piper.AdditionalChallenge != nil {

		for {
			err := d.transport.writePacket(Marshal(&userAuthFailureMsg{
				Methods: []string{"keyboard-interactive"},
			}))

			if err != nil {
				return err
			}

			userAuthReq, err := d.nextAuthMsg()

			if err != nil {
				return err
			}

			if userAuthReq.Method == "keyboard-interactive" {
				break
			}
		}

		prompter := &sshClientKeyboardInteractive{d.connection}
		ok, err := piper.AdditionalChallenge(d, prompter.Challenge)

		if err != nil {
			return err
		}

		if !ok {
			return fmt.Errorf("additional challenge failed")
		}
	}

	upconn, upconfig, err := piper.FindUpstream(d)
	if err != nil {
		return err
	}

	addr := upconn.RemoteAddr().String()

	u, err := newUpstream(upconn, addr, upconfig)
	if err != nil {
		return err
	}
	defer u.Close()

	p := &pipedConn{
		upstream:   u,
		downstream: d,
	}

	p.processAuthMsg = func(msg *userAuthRequestMsg) (*userAuthRequestMsg, error) {

		// only public msg need
		if msg.Method != "publickey" {
			return msg, nil
		}

		user := msg.User
		// pubKey MAP
		downKey, isQuery, sig, err := parsePublicKeyMsg(msg)
		if err != nil {
			return nil, err
		}

		signer, err := piper.MapPublicKey(d, downKey)

		// no mapped user change it to none or error occur
		if err != nil || signer == nil {
			return noneAuthMsg(user), nil
		}

		upKey := signer.PublicKey()

		if isQuery {
			// reply for query msg
			msg, err = p.validAndAck(upKey, downKey)
		} else {

			ok, err := p.checkPublicKey(msg, downKey, sig)

			if err != nil {
				return nil, err
			}

			if !ok {
				return noneAuthMsg(user), nil
			}

			msg, err = p.signAgain(msg, signer, downKey)
		}

		if err != nil {
			return nil, err
		}

		return msg, nil
	}

	err = p.pipeAuth(userAuthReq)
	if err != nil {
		return err
	}

	// block until connection closed or errors occur
	return p.loop()
}

func (pipe *pipedConn) validAndAck(upKey, downKey PublicKey) (*userAuthRequestMsg, error) {

	user := pipe.downstream.User()
	ok, err := validateKey(upKey, user, pipe.upstream.transport)

	if ok {
		okMsg := userAuthPubKeyOkMsg{
			Algo:   downKey.Type(),
			PubKey: downKey.Marshal(),
		}

		if err = pipe.downstream.transport.writePacket(Marshal(&okMsg)); err != nil {
			return nil, err
		}

		return nil, nil
	}

	return noneAuthMsg(user), nil
}

func (pipe *pipedConn) checkPublicKey(msg *userAuthRequestMsg, pubkey PublicKey, sig *Signature) (bool, error) {

	if !isAcceptableAlgo(sig.Format) {
		return false, nil
	}
	signedData := buildDataSignedForAuth(pipe.downstream.transport.getSessionID(), *msg, []byte(pubkey.Type()), pubkey.Marshal())

	if err := pubkey.Verify(signedData, sig); err != nil {
		return false, nil
	}

	return true, nil
}

func (pipe *pipedConn) signAgain(msg *userAuthRequestMsg, signer Signer, downKey PublicKey) (*userAuthRequestMsg, error) {

	user := pipe.downstream.User()

	rand := pipe.upstream.transport.config.Rand
	session := pipe.upstream.transport.getSessionID()

	upKey := signer.PublicKey()
	upKeyData := upKey.Marshal()

	sign, err := signer.Sign(rand, buildDataSignedForAuth(session, userAuthRequestMsg{
		User:    user,
		Service: serviceSSH,
		Method:  "publickey",
	}, []byte(upKey.Type()), upKeyData))
	if err != nil {
		return nil, err
	}

	// manually wrap the serialized signature in a string
	s := Marshal(sign)
	sig := make([]byte, stringLength(len(s)))
	marshalString(sig, s)

	pubkeyMsg := &publickeyAuthMsg{
		User:     user,
		Service:  serviceSSH,
		Method:   "publickey",
		HasSig:   true,
		Algoname: upKey.Type(),
		PubKey:   upKeyData,
		Sig:      sig,
	}

	Unmarshal(Marshal(pubkeyMsg), msg)

	return msg, nil
}

func parsePublicKeyMsg(userAuthReq *userAuthRequestMsg) (PublicKey, bool, *Signature, error) {
	if userAuthReq.Method != "publickey" {
		return nil, false, nil, fmt.Errorf("not a publickey auth msg")
	}

	payload := userAuthReq.Payload
	if len(payload) < 1 {
		return nil, false, nil, parseError(msgUserAuthRequest)
	}
	isQuery := payload[0] == 0
	payload = payload[1:]
	algoBytes, payload, ok := parseString(payload)
	if !ok {
		return nil, false, nil, parseError(msgUserAuthRequest)
	}
	algo := string(algoBytes)
	if !isAcceptableAlgo(algo) {
		return nil, false, nil, fmt.Errorf("ssh: algorithm %q not accepted", algo)
	}

	pubKeyData, payload, ok := parseString(payload)
	if !ok {
		return nil, false, nil, parseError(msgUserAuthRequest)
	}

	pubKey, err := ParsePublicKey(pubKeyData)
	if err != nil {
		return nil, false, nil, err
	}

	var sig *Signature
	if !isQuery {
		sig, payload, ok = parseSignature(payload)
		if !ok || len(payload) > 0 {
			return nil, false, nil, parseError(msgUserAuthRequest)
		}
	}

	return pubKey, isQuery, sig, nil
}

func piping(dst, src packetConn) error {
	for {
		p, err := src.readPacket()

		if err != nil {
			return err
		}

		err = dst.writePacket(p)

		if err != nil {
			return err
		}
	}
}

func (pipe *pipedConn) loop() error {
	c := make(chan error)

	go func() {
		c <- piping(pipe.upstream.mux.conn, pipe.downstream.mux.conn)
	}()

	go func() {
		c <- piping(pipe.downstream.mux.conn, pipe.upstream.mux.conn)
	}()

	defer pipe.Close()

	// wait until either connection closed
	return <-c
}

func (pipe *pipedConn) Close() {
	pipe.upstream.mux.conn.Close()
	pipe.downstream.mux.conn.Close()
}

func (pipe *pipedConn) pipeAuth(initUserAuthMsg *userAuthRequestMsg) error {
	err := pipe.upstream.sendAuthReq()
	if err != nil {
		return err
	}

	userAuthMsg := initUserAuthMsg

	for {
		// hook msg
		userAuthMsg, err = pipe.processAuthMsg(userAuthMsg)

		if err != nil {
			return err
		}

		// nil for ignore
		if userAuthMsg != nil {
			err = pipe.upstream.transport.writePacket(Marshal(userAuthMsg))
			if err != nil {
				return err
			}

			packet, err := pipe.upstream.transport.readPacket()
			if err != nil {
				return err
			}

			success := packet[0] == msgUserAuthSuccess

			if err = pipe.downstream.transport.writePacket(packet); err != nil {
				return err
			}

			if success {
				return nil
			}
		}

		userAuthMsg, err = pipe.downstream.nextAuthMsg()
		if err != nil {
			return err
		}

	}
}

func (u *upstream) sendAuthReq() error {
	if err := u.transport.writePacket(Marshal(&serviceRequestMsg{serviceUserAuth})); err != nil {
		return err
	}

	packet, err := u.transport.readPacket()
	if err != nil {
		return err
	}
	var serviceAccept serviceAcceptMsg
	if err := Unmarshal(packet, &serviceAccept); err != nil {
		return err
	}

	return nil
}

func newDownstream(c net.Conn, config *ServerConfig) (*downstream, error) {
	fullConf := *config
	fullConf.SetDefaults()

	s := &connection{
		sshConn: sshConn{conn: c},
	}

	_, err := s.serverHandshake(&fullConf)
	if err != nil {
		c.Close()
		return nil, err
	}

	return &downstream{s}, nil
}

func newUpstream(c net.Conn, addr string, config *ClientConfig) (*upstream, error) {
	fullConf := *config
	fullConf.SetDefaults()

	conn := &connection{
		sshConn: sshConn{conn: c},
	}

	if err := conn.clientHandshake(addr, &fullConf); err != nil {
		c.Close()
		return nil, err
	}
	conn.mux = newMux(conn.transport)

	return &upstream{conn}, nil
}

func (d *downstream) nextAuthMsg() (*userAuthRequestMsg, error) {
	var userAuthReq userAuthRequestMsg

	if packet, err := d.transport.readPacket(); err != nil {
		return nil, err
	} else if err = Unmarshal(packet, &userAuthReq); err != nil {
		return nil, err
	}

	if userAuthReq.Service != serviceSSH {
		return nil, errors.New("ssh: client attempted to negotiate for unknown service: " + userAuthReq.Service)
	}

	return &userAuthReq, nil
}

func noneAuthMsg(user string) *userAuthRequestMsg {
	return &userAuthRequestMsg{
		User:    user,
		Service: serviceSSH,
		Method:  "none",
	}
}
