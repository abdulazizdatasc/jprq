package main

import (
	"errors"
	"fmt"
	"github.com/azimjohn/jprq/server/config"
	"github.com/azimjohn/jprq/server/events"
	"github.com/azimjohn/jprq/server/github"
	"github.com/azimjohn/jprq/server/server"
	"github.com/azimjohn/jprq/server/tunnel"
	"io"
	"net"
	"strings"
	"time"
)

const dateFormat = "2006/01/02 15:04:05"

type Jprq struct {
	config          config.Config
	eventServer     server.TCPServer
	publicServer    server.TCPServer
	publicServerTLS server.TCPServer
	blockedUsers    map[string]string
	authenticator   github.Authenticator
	tcpTunnels      map[uint16]*tunnel.TCPTunnel
	httpTunnels     map[string]*tunnel.HTTPTunnel
	userTunnels     map[string]map[string]tunnel.Tunnel
}

func (j *Jprq) Init(conf config.Config, oauth github.Authenticator) error {
	j.config = conf
	j.authenticator = oauth
	j.tcpTunnels = make(map[uint16]*tunnel.TCPTunnel)
	j.httpTunnels = make(map[string]*tunnel.HTTPTunnel)
	j.userTunnels = make(map[string]map[string]tunnel.Tunnel)

	if err := j.eventServer.Init(conf.EventServerPort, "jprq_event_server"); err != nil {
		return err
	}
	if err := j.publicServer.Init(conf.PublicServerPort, "jprq_public_server"); err != nil {
		return err
	}
	if err := j.publicServerTLS.InitTLS(conf.PublicServerTLSPort, "jprq_public_server_tls", conf.TLSCertFile,
		conf.TLSKeyFile); err != nil {
		return err
	}
	return nil
}

func (j *Jprq) Start() {
	go j.eventServer.Start(j.serveEventConn)
	go j.publicServer.Start(j.servePublicConn)
	go j.publicServerTLS.Start(j.servePublicConn)
}

func (j *Jprq) Stop() error {
	if err := j.eventServer.Stop(); err != nil {
		return err
	}
	if err := j.publicServer.Stop(); err != nil {
		return err
	}
	if err := j.publicServerTLS.Stop(); err != nil {
		return err
	}
	return nil
}

func (j *Jprq) servePublicConn(conn net.Conn) error {
	host, buffer, err := parseHost(conn)
	if err != nil || host == "" {
		writeResponse(conn, 400, "Bad Request", "Bad Request")
		return nil
	}
	host = strings.ToLower(host)
	t, found := j.httpTunnels[host]
	if !found {
		writeResponse(conn, 404, "Not Found", "tunnel not found. create one at jprq.io")
		return errors.New(fmt.Sprintf("unknown host requested %s", host))
	}
	return t.PublicConnectionHandler(conn, buffer)
}

func (j *Jprq) serveEventConn(conn net.Conn) error {
	defer conn.Close()

	var event events.Event[events.TunnelRequested]
	if err := event.Read(conn); err != nil {
		return err
	}

	request := event.Data
	if request.Protocol != events.HTTP && request.Protocol != events.TCP {
		return events.WriteError(conn, "invalid protocol %s", request.Protocol)
	}
	user, err := j.authenticator.Authenticate(request.AuthToken)
	if err != nil {
		return events.WriteError(conn, "authentication failed")
	}
	if reason, found := j.blockedUsers[user.Login]; found {
		return events.WriteError(conn, "account blocked for %s", reason)
	}
	if len(j.userTunnels[user.Login]) >= j.config.MaxTunnelsPerUser {
		return events.WriteError(conn, "tunnels limit reached for %s", user.Login)
	}
	if request.Subdomain == "" {
		request.Subdomain = user.Login
	}
	if err := validate(request.Subdomain); err != nil {
		return events.WriteError(conn, "invalid subdomain %s: %s", request.Subdomain, err.Error())
	}
	hostname := fmt.Sprintf("%s.%s", request.Subdomain, j.config.DomainName)
	if _, ok := j.httpTunnels[hostname]; ok {
		return events.WriteError(conn, "subdomain is busy: %s, try another one", request.Subdomain)
	}

	var t tunnel.Tunnel
	var maxConsLimit = j.config.MaxConsPerTunnel

	switch request.Protocol {
	case events.HTTP:
		tn, err := tunnel.NewHTTP(hostname, conn, maxConsLimit)
		if err != nil {
			return events.WriteError(conn, "failed to create http tunnel", err.Error())
		}
		j.httpTunnels[hostname] = tn
		defer delete(j.httpTunnels, hostname)
		t = tn
	case events.TCP:
		tn, err := tunnel.NewTCP(hostname, conn, maxConsLimit)
		if err != nil {
			return events.WriteError(conn, "failed to create tcp tunnel", err.Error())
		}
		j.tcpTunnels[tn.PublicServerPort()] = tn
		defer delete(j.tcpTunnels, tn.PublicServerPort())
		t = tn
	}

	tunnelId := fmt.Sprintf("%s:%d", t.Hostname(), t.PublicServerPort())
	if len(j.userTunnels[user.Login]) == 0 {
		j.userTunnels[user.Login] = make(map[string]tunnel.Tunnel)
	}
	j.userTunnels[user.Login][tunnelId] = t
	defer delete(j.userTunnels[user.Login], tunnelId)

	t.Open()
	defer t.Close()
	opened := events.Event[events.TunnelOpened]{
		Data: &events.TunnelOpened{
			Hostname:      t.Hostname(),
			Protocol:      t.Protocol(),
			PublicServer:  t.PublicServerPort(),
			PrivateServer: t.PrivateServerPort(),
		},
	}
	if err := opened.Write(conn); err != nil {
		return err
	}

	fmt.Printf("%s [tunnel-opened] %s: %s\n", time.Now().Format(dateFormat), user.Login, tunnelId)
	buffer := make([]byte, 8) // wait until connection is closed
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		if _, err := conn.Read(buffer); err == io.EOF {
			break
		}
		if _, found := j.blockedUsers[user.Login]; found {
			break
		}
	}
	fmt.Printf("%s [tunnel-closed] %s: %s\n", time.Now().Format(dateFormat), user.Login, tunnelId)
	return nil
}
