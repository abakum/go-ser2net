package ser2net

// NAWS - Negotiate About Window Size - https://tools.ietf.org/html/rfc1073

import (
	"bytes"
	"encoding/binary"
	"log"
	"time"

	"github.com/PatrickRudolph/telnet"
)

// NAWSOption enables NAWS negotiation on a Server.
func (l *likeSerialPort) Server1073(c *telnet.Connection) telnet.Negotiator {
	return l
}

// ExposeNAWS enables NAWS negotiation on a Client.
func (l *likeSerialPort) Client1073(c *telnet.Connection) telnet.Negotiator {
	l.ws, _ = size()
	return l
}

// OptionCode returns the IAC code for NAWS.
func (l *likeSerialPort) OptionCode() byte {
	return telnet.TeloptNAWS
}

// Offer sends the IAC WILL NAWS command to the server.
func (l *likeSerialPort) Offer(c *telnet.Connection) {
	if l.console == nil {
		// Клиент.
		c.Conn.Write([]byte{telnet.IAC, telnet.WILL, l.OptionCode()})
	}
}

// HandleWill sends the IAC DO NAWS command to the client.
func (l *likeSerialPort) HandleWill(c *telnet.Connection) {
	if l.console != nil {
		// Сервер.
		c.Conn.Write([]byte{telnet.IAC, telnet.DO, l.OptionCode()})
	}
}

// HandleDo processes the monitor size options for NAWS.
func (l *likeSerialPort) HandleDo(c *telnet.Connection) {
	if l.console == nil {
		// Клиент.
		l.command = true
		c.Conn.Write([]byte{telnet.IAC, telnet.WILL, l.OptionCode()})
		l.writeSize(c)
		go l.monitorTTYSize(c)
		// } else {
		// 	c.Conn.Write([]byte{telnet.IAC, telnet.WONT, l.OptionCode()})
	}
}

func (l *likeSerialPort) monitorTTYSize(c *telnet.Connection) {
	// Клиент из HandleDo
	t := time.NewTicker(time.Second)
	for range t.C {
		if l.closed {
			t.Stop()
			return
		}
		ws, err := size()
		if err != nil {
			continue
		}
		if ws.Width != l.ws.Width || ws.Height != l.ws.Height {
			l.ws = ws
			l.writeSize(c)
		}
	}
}

func (l *likeSerialPort) writeSize(c *telnet.Connection) {
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, l.OptionCode()})
	payload := new(bytes.Buffer)
	binary.Write(payload, binary.BigEndian, l.ws.Width)
	binary.Write(payload, binary.BigEndian, l.ws.Height)
	b.Write(escapeIAC(payload.Bytes()))

	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err := c.Conn.Write(b.Bytes())
	// w.setEnable(c, err == nil)
	log.Printf("%s->%s %s %dx%d %v\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(l.OptionCode()), l.ws.Width, l.ws.Height, err)
}

// HandleSB processes the information about window size sent from the client to the server.
func (l *likeSerialPort) HandleSB(c *telnet.Connection, b []byte) {
	if len(b) != 4 {
		// Спам.
		return
	}
	if l.console != nil {
		// Сервер.
		l.ws.Width = binary.BigEndian.Uint16(b[0:2])
		l.ws.Height = binary.BigEndian.Uint16(b[2:4])
		l.console.SetSize(int(l.ws.Width), int(l.ws.Height))
	}
}
