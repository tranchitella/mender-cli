// Copyright 2020 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package cmd

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/vmihailenco/msgpack"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/mendersoftware/go-lib-micro/ws"
	wsshell "github.com/mendersoftware/go-lib-micro/ws/shell"

	"github.com/mendersoftware/mender-cli/log"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 1 * time.Second

	// Maximum message size allowed from peer.
	maxMessageSize = 8192

	// Time allowed to read the next pong message from the peer.
	defaultPingWait = 1 * time.Minute

	// protocols
	httpsProtocol = "https"
	httpProtocol  = "http"
	wssProtocol   = "wss"
	wsProtocol    = "ws"
)

var terminalCmd = &cobra.Command{
	Use:   "terminal DEVICE_ID",
	Short: "Access a device's remote terminal",
	Args:  cobra.ExactArgs(1),
	Run: func(c *cobra.Command, args []string) {
		cmd, err := NewTerminalCmd(c, args)
		CheckErr(err)
		CheckErr(cmd.Run())
	},
}

func getWebSocketScheme(scheme string) string {
	if scheme == httpsProtocol {
		scheme = wssProtocol
	} else if scheme == httpProtocol {
		scheme = wsProtocol
	}
	return scheme
}

// TerminalCmd handles the terminal command
type TerminalCmd struct {
	server     string
	skipVerify bool
	deviceID   string
	sessionID  string
	running    bool
	stop       chan bool
}

// NewTerminalCmd returns a new TerminalCmd
func NewTerminalCmd(cmd *cobra.Command, args []string) (*TerminalCmd, error) {
	server, err := cmd.Flags().GetString(argRootServer)
	if err != nil {
		return nil, err
	}

	skipVerify, err := cmd.Flags().GetBool(argRootSkipVerify)
	if err != nil {
		return nil, err
	}

	return &TerminalCmd{
		server:     server,
		skipVerify: skipVerify,
		deviceID:   args[0],
		stop:       make(chan bool),
	}, nil
}

// Run executes the command
func (c *TerminalCmd) Run() error {
	log.Info(fmt.Sprintf("Connecting to the remote terminal of the device %s...", c.deviceID))

	tokenPath, err := getDefaultAuthTokenPath()
	if err != nil {
		return errors.Wrap(err, "Unable to determine the auth token path")
	}

	token, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		return errors.Wrap(err, "Please Login first")
	}

	// connect to the websocket
	deviceConnectPath := "/api/management/v1/deviceconnect/devices/" + c.deviceID + "/connect"
	parsedURL, err := url.Parse(c.server)
	if err != nil {
		return err
	}

	scheme := getWebSocketScheme(parsedURL.Scheme)
	u := url.URL{Scheme: scheme, Host: parsedURL.Host, Path: deviceConnectPath}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+string(token))
	websocket.DefaultDialer.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: c.skipVerify,
	}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		return err
	}
	defer conn.Close()

	// ping-pong
	conn.SetReadLimit(maxMessageSize)
	_ = conn.SetReadDeadline(time.Now().Add(defaultPingWait))
	conn.SetPingHandler(func(message string) error {
		pongWait, _ := strconv.Atoi(message)
		_ = conn.SetReadDeadline(time.Now().Add(time.Duration(pongWait) * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte{}, time.Now().Add(writeWait))
	})

	log.Info("Press CTRL+] to quit the session")

	// set the terminal in raw mode
	oldState, err := term.MakeRaw(0)
	if err != nil {
		return err
	}
	defer func() {
		_ = term.Restore(0, oldState)
	}()

	// get the terminal width and height
	termID := int(os.Stdout.Fd())
	termWidth, termHeight, err := terminal.GetSize(termID)
	if err != nil {
		return err
	}

	// send the shell start message
	m := &ws.ProtoMsg{
		Header: ws.ProtoHdr{
			Proto:   ws.ProtoTypeShell,
			MsgType: wsshell.MessageTypeSpawnShell,
			Properties: map[string]interface{}{
				"terminal_width":  termWidth,
				"terminal_height": termHeight,
			},
		},
	}

	data, err := msgpack.Marshal(m)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteMessage(websocket.BinaryMessage, data)

	c.running = true
	go c.pipeStdout(conn, os.Stdout)
	go c.pipeStdin(conn, os.Stdin)

	// handle CTRL+C and signals
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, unix.SIGINT, unix.SIGTERM)

	// resize the terminal window
	ticker := time.NewTicker(500 * time.Millisecond)
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				newTermWidth, newTermHeight, _ := terminal.GetSize(termID)
				if newTermWidth != termWidth || newTermHeight != termHeight {
					termWidth = newTermWidth
					termHeight = newTermHeight
					m := &ws.ProtoMsg{
						Header: ws.ProtoHdr{
							Proto:   ws.ProtoTypeShell,
							MsgType: wsshell.MessageTypeResizeShell,
							Properties: map[string]interface{}{
								"terminal_width":  termWidth,
								"terminal_height": termHeight,
							},
						},
					}
					if data, err := msgpack.Marshal(m); err == nil {
						_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
						_ = conn.WriteMessage(websocket.BinaryMessage, data)
					}
				}
			}
		}
	}()

	// wait for CTRL+C, signals or stop
	select {
	case <-interrupt:
	case <-quit:
	case <-c.stop:
	}

	// stop the ticker
	ticker.Stop()
	select {
	case done <- true:
	default:
	}

	m = &ws.ProtoMsg{
		Header: ws.ProtoHdr{
			Proto:     ws.ProtoTypeShell,
			MsgType:   wsshell.MessageTypeStopShell,
			SessionID: c.sessionID,
		},
	}

	data, err = msgpack.Marshal(m)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteMessage(websocket.BinaryMessage, data)
	conn.Close()

	return nil
}

func (c *TerminalCmd) Stop() {
	c.stop <- true
	c.running = false
}

func (c *TerminalCmd) pipeStdout(conn *websocket.Conn, r io.Reader) {
	s := bufio.NewReader(r)
	for c.running {
		raw := make([]byte, 1024)
		n, err := s.Read(raw)
		if err != nil {
			if c.running {
				log.Err(fmt.Sprintf("error: %v", err))
			}
			break
		}
		// CLTR+], terminate the shell
		if raw[0] == 29 {
			c.Stop()
			return
		}

		m := &ws.ProtoMsg{
			Header: ws.ProtoHdr{
				Proto:     ws.ProtoTypeShell,
				MsgType:   wsshell.MessageTypeShellCommand,
				SessionID: c.sessionID,
			},
			Body: raw[:n],
		}

		data, err := msgpack.Marshal(m)
		if err != nil {
			log.Err(fmt.Sprintf("error: %v", err))
			break
		}

		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		_ = conn.WriteMessage(websocket.BinaryMessage, data)
	}
}

func (c *TerminalCmd) pipeStdin(conn *websocket.Conn, w io.Writer) {
	for c.running {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if c.running {
				log.Err(fmt.Sprintf("error: %v", err))
			}
			break
		}

		m := &ws.ProtoMsg{}
		err = msgpack.Unmarshal(data, m)
		if err != nil {
			log.Err(fmt.Sprintf("error: %v", err))
			break
		}
		if m.Header.Proto == ws.ProtoTypeShell && m.Header.MsgType == wsshell.MessageTypeShellCommand {
			if _, err := w.Write(m.Body); err != nil {
				break
			}
		} else if m.Header.Proto == ws.ProtoTypeShell && m.Header.MsgType == wsshell.MessageTypeSpawnShell {
			c.sessionID = string(m.Header.SessionID)
		}
	}
}
