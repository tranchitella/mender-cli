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
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/vmihailenco/msgpack"

	"github.com/mendersoftware/go-lib-micro/ws"
	"github.com/mendersoftware/go-lib-micro/ws/filetransfer"

	"github.com/mendersoftware/mender-cli/log"
)

const (
	deviceDelimiter = ":"
)

var fileTransferCmd = &cobra.Command{
	Use:   "cp [file_path|device_id:file_path] [file_path|device_id:file_path]",
	Short: "Transfer files from/to device",
	Args:  cobra.MinimumNArgs(2),
	Run: func(c *cobra.Command, args []string) {
		cmd, err := NewFileTransfer(c, args)
		CheckErr(err)
		CheckErr(cmd.Run())
	},
}

func init() {
	//fileTransferCmd.Flags().StringP(argRecord, "", "", "recording file path to save the session to")
	//fileTransferCmd.Flags().StringP(argPlayback, "", "", "recording file path to playback the session from")
}

// TerminalCmd handles the terminal command
type NewFileTransferCmd struct {
	server      string
	skipVerify  bool
	source      string
	destination string
	sessionID   string
}

// NewFileTransferCmd returns a new NewFileTransferCmd
func NewFileTransfer(cmd *cobra.Command, args []string) (*NewFileTransferCmd, error) {
	server, err := cmd.Flags().GetString(argRootServer)
	if err != nil {
		return nil, err
	}

	skipVerify, err := cmd.Flags().GetBool(argRootSkipVerify)
	if err != nil {
		return nil, err
	}

	return &NewFileTransferCmd{
		server:      server,
		skipVerify:  skipVerify,
		source:      args[0],
		destination: args[1],
	}, nil
}

//can pare one of the formats:
//1. p===device_id:/absolute/path/somewhere
//2. p===/any/path/also/relative
func parsePath(p string) (dId string, fPath string, err error) {
	if strings.Contains(p, deviceDelimiter) {
		a := strings.Split(p, deviceDelimiter)
		return a[0], a[1], nil
	} else {
		return "", p, nil
	}
}

// Run executes the command
func (c *NewFileTransferCmd) Run() error {
	deviceIdSource, filePathSource, err := parsePath(c.source)
	deviceIdDestination, filePathDestination, err := parsePath(c.destination)

	if len(deviceIdSource) < 1 {
		log.Err(fmt.Sprintf("device id was not given in source:%s", c.source))
		return errors.New("for now we support cp from device, please provide device id")
	}

	if len(deviceIdDestination) > 0 {
		log.Err(fmt.Sprintf("device id was given in destination:%s", c.destination))
		return errors.New("for now we support cp from device, please provide local destination path")
	}

	if len(filePathSource) < 1 {
		log.Err(fmt.Sprintf("file path was not given in source:%s", c.source))
		return errors.New("we need something to copy, please provide a file path")
	}

	if len(filePathDestination) < 1 {
		log.Err(fmt.Sprintf("file path was not given in destination:%s", c.source))
		return errors.New("we need a place to copy to, please provide a destination file path")
	}

	log.Info(fmt.Sprintf("Connecting to the remote terminal of the device %s", deviceIdSource))

	tokenPath, err := getDefaultAuthTokenPath()
	if err != nil {
		return errors.Wrap(err, "Unable to determine the auth token path")
	}

	token, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		return errors.Wrap(err, "Please Login first")
	}

	// connect to the websocket
	deviceConnectPath := "/api/management/v1/deviceconnect/devices/" + deviceIdSource + "/connect"
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

	// send the file_stat message
	m := &ws.ProtoMsg{
		Header: ws.ProtoHdr{
			Proto:      ws.ProtoTypeFileTransfer,
			MsgType:    filetransfer.MessageTypeStatFile,
			Properties: map[string]interface{}{},
		},
		Body: []byte(filePathSource),
	}

	data, err := msgpack.Marshal(m)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	err = conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		log.Err(fmt.Sprintf("failed to write StatFile message: %s", err.Error()))
		return err
	}

	_, data, err = conn.ReadMessage()
	if err != nil {
		log.Err(fmt.Sprintf("error reading response for StatFile message: %v", err))
		return err
	}

	m = &ws.ProtoMsg{}
	err = msgpack.Unmarshal(data, m)
	if err != nil {
		log.Err(fmt.Sprintf("error unmarshalling StatFIle response message: %v", err))
		return nil
	}

	log.Info(fmt.Sprintf("got stat_file: %+v", m))
	fileSize:=m.Header.Properties["file_size"].(int64)

	// send the get_file message
	m = &ws.ProtoMsg{
		Header: ws.ProtoHdr{
			Proto:      ws.ProtoTypeFileTransfer,
			MsgType:    filetransfer.MessageTypeGetFile,
			Properties: map[string]interface{}{},
		},
		Body: []byte(filePathSource),
	}

	data, err = msgpack.Marshal(m)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	err = conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		log.Err(fmt.Sprintf("failed to write GetFile message: %s", err.Error()))
		return err
	}

	_,err=os.Stat(filePathDestination)
	if err==nil {
		log.Err(fmt.Sprintf("destiantion path exists: %s", filePathDestination))
		return nil
	}

	destination,err:=os.Create(filePathDestination)
	if err!=nil {
		log.Err(fmt.Sprintf("cannot create: %s", filePathDestination))
		return nil
	}
	defer destination.Close()

	t0:=time.Now()
	var readSize int64 = 0
	for {
		_, data, err = conn.ReadMessage()
		if err != nil {
			log.Err(fmt.Sprintf("error reading response for StatFile message: %v", err))
			return err
		}

		m = &ws.ProtoMsg{}
		err = msgpack.Unmarshal(data, m)
		if err != nil {
			log.Err(fmt.Sprintf("error unmarshalling StatFIle response message: %v", err))
			return err
		}
		destination.Write(m.Body)
		log.Info(fmt.Sprintf("got get_file %d/%d chunk len=%d", readSize, fileSize, len(m.Body)))
		readSize+=int64(len(m.Body))
		if readSize >= fileSize {
			break
		}
	}
	t1:=time.Now()
	dt:=t1.UnixNano()-t0.UnixNano()
	log.Info(fmt.Sprintf("transferred %s in %.2f seconds.",filePathSource,float64(dt)*0.000000001))

	return nil
}

func (c *NewFileTransferCmd) Stop() {
}
