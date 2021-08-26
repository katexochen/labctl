package transport

import (
	"fmt"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/types"
	"golang.org/x/crypto/ssh"
)

// Debug count
var DebugCount int

type SSHTransportOption func(*SSHTransport) error

// SSHTransport setting needs to be set before calling Connect()
// SSHTransport implements the Transport interface
type SSHTransport struct {
	// Channel used to read. Can use Expect to Write & read with timeout
	in chan SSHReply
	// SSH Session
	ses *SSHSession
	// Contains the first read after connecting
	LoginMessage *SSHReply
	// SSH parameters used in connect
	// default: 22
	Port int

	// Keep the target for logging
	Target string

	// SSH Options
	// required!
	SSHConfig *ssh.ClientConfig

	// Character to split the incoming stream (#/$/>)
	// default: #
	PromptChar string

	// Kind specific transactions & prompt checking function
	K SSHKind
}

// Add username & password authentication
func WithUserNamePassword(username, password string) SSHTransportOption {
	return func(tx *SSHTransport) error {
		tx.SSHConfig.User = username
		if tx.SSHConfig.Auth == nil {
			tx.SSHConfig.Auth = []ssh.AuthMethod{}
		}
		tx.SSHConfig.Auth = append(tx.SSHConfig.Auth, ssh.Password(password))
		return nil
	}
}

// Add a basic username & password to a config
// Will initialize the config if required
func HostKeyCallback(callback ...ssh.HostKeyCallback) SSHTransportOption {
	return func(tx *SSHTransport) error {
		tx.SSHConfig.HostKeyCallback = func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if len(callback) == 0 {
				log.Debugf("Skipping host key verification for %s", hostname)
				return nil
			}
			for _, hkc := range callback {
				if hkc(hostname, remote, key) == nil {
					return nil
				}
			}
			return fmt.Errorf("invalid host key %s: %s", hostname, key)
		}
		return nil
	}
}

func NewSSHTransport(node *types.NodeConfig, options ...SSHTransportOption) (*SSHTransport, error) {
	switch node.Kind {
	case "vr-sros", "srl":
		c := &SSHTransport{}
		c.SSHConfig = &ssh.ClientConfig{}

		// apply options
		for _, opt := range options {
			err := opt(c)
			if err != nil {
				return nil, err
			}
		}

		switch node.Kind {
		case "vr-sros":
			c.K = &SSHKindVRSROS{}
		case "srl":
			c.K = &SSHKindSRL{}
		}
		return c, nil
	}
	return nil, fmt.Errorf("no transport implemented for kind: %s", node.Kind)
}

// Creates the channel reading the SSH connection
//
// The first prompt is saved in LoginMessages
//
// - The channel read the SSH session, splits on PromptChar
// - Uses SSHKind's PromptParse to split the received data in *result* and *prompt* parts
//   (if no valid prompt was found, prompt will simply be empty and result contain all the data)
// - Emit data
func (t *SSHTransport) InChannel() {
	// Ensure we have a working channel
	t.in = make(chan SSHReply)

	// setup a buffered string channel
	go func() {
		buf := make([]byte, 1024)
		tmpS := ""
		n, err := t.ses.In.Read(buf) //this reads the ssh terminal
		if err == nil {
			tmpS = string(buf[:n])
		}
		for err == nil {

			if strings.Contains(tmpS, "#") {
				parts := strings.Split(tmpS, "#")
				li := len(parts) - 1
				for i := 0; i < li; i++ {
					r := t.K.PromptParse(t, &parts[i])
					if r == nil {
						r = &SSHReply{
							Result: parts[i],
						}
					}
					t.in <- *r
				}
				tmpS = parts[li]
			}
			n, err = t.ses.In.Read(buf)
			tmpS += string(buf[:n])
		}
		log.Debugf("In Channel closing: %v", err)
		t.in <- SSHReply{
			Result: tmpS,
			Prompt: "",
		}
	}()

	// Save first prompt
	t.LoginMessage = t.Run("", 15)
	if DebugCount > 2 {
		t.LoginMessage.Log(t.Target)
	}
}

// Run a single command and wait for the reply
func (t *SSHTransport) Run(command string, timeout time.Duration) *SSHReply {
	if command != "" {
		_, err := t.ses.Writeln(command)
		if err != nil {
			log.Errorf("unable to send command: %s: %s", command, err)
		}
		log.Debugf("--> %s\n", command)
		time.Sleep(time.Duration(10) * time.Millisecond)
	}

	var sHistory string

	for {
		// Read from the channel with a timeout
		var rr string

		start := time.Now()

		select {
		case <-time.After(timeout * time.Second):
			log.Debugf("timeout waiting for prompt: %s, %v (%d)", command, time.Since(start), timeout)
			return &SSHReply{
				Result:  sHistory,
				Command: command,
			}
		case ret := <-t.in:
			if DebugCount > 1 {
				ret.Debug(t.Target, command+"<--InChannel--")
			}

			if ret.Result == "" && ret.Prompt == "" {
				log.Debugf("received zero?")
				continue
			}

			if ret.Prompt == "" && ret.Result != "" {
				// we should continue reading...
				sHistory += ret.Result
				if DebugCount > 1 {
					log.Debugf("+")
				}
				//timeout = 2 // reduce timeout, node is already sending data
				continue
			}

			if sHistory == "" {
				rr = ret.Result
			} else {
				rr = sHistory + "#" + ret.Result
				sHistory = ""
			}
			rr = strings.Trim(rr, " \n\r\t")

			if strings.HasPrefix(rr, command) {
				rr = strings.Trim(rr[len(command):], " \n\r\t")
			} else if !strings.Contains(rr, command) {
				log.Debugf("read more %s:%s", command, rr)
				sHistory = rr
				continue
			}
			res := &SSHReply{
				Result:  rr,
				Prompt:  ret.Prompt,
				Command: command,
			}
			res.Debug(t.Target, command+"<--RUN--")
			return res
		}
	}
}

func (t *SSHTransport) Write(data *string) (int, []*SSHReply, error) {
	c := 0
	var res []*SSHReply
	for _, l := range strings.Split(*data, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		c++
		r := t.Run(l, 5).Log(t.Target, log.WarnLevel)
		if r.Result != "" {
			res = append(res, r)
		}
	}
	return c, res, nil
}

// Connect to a host
// Part of the Transport interface
func (t *SSHTransport) Connect(host string) error {
	// Assign Default Values
	if t.PromptChar == "" {
		t.PromptChar = "#"
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if t.SSHConfig == nil {
		return fmt.Errorf("require auth credentials in SSHConfig")
	}

	// Start some client config
	host = fmt.Sprintf("%s:%d", host, t.Port)

	ses_, err := NewSSHSession(host, t.SSHConfig)
	if err != nil || ses_ == nil {
		return fmt.Errorf("cannot connect to %s: %s", host, err)
	}
	t.ses = ses_

	log.Infof("%s: connected to %s\n", t.Target, host)
	t.InChannel()
	//Read to first prompt
	return nil
}

// Close the Session and channels
// Part of the Transport interface
func (t *SSHTransport) Close() {
	if t.in != nil {
		close(t.in)
		t.in = nil
	}
	t.ses.Close()
}
