package ser2net

// NAWS - Negotiate About Window Size - https://tools.ietf.org/html/rfc1073

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"time"

	"github.com/PatrickRudolph/telnet"
)

// Путь к команде.
var (
	path string          // Сервер
	ctx  context.Context // Клиент
)

// Server1073 enables NAWS negotiation on a Server.
func (w *SerialWorker) Server1073(c *telnet.Connection) telnet.Negotiator {
	path = "$" + w.path // $ Чтоб отличить команду от последовтельной консоли
	W, H, _ := w.like.console.GetSize()
	log.Printf("Telnet server %s accepted connection from %s. WxH: %dx%d\r\n", c.LocalAddr(), c.RemoteAddr(), W, H)
	return w.like
}

// Client1073 enables NAWS negotiation on a Client.
func (w *SerialWorker) Client1073(c *telnet.Connection) telnet.Negotiator {
	if w.like == nil {
		w.like = &likeSerialPort{}
	}
	ctx = w.context
	w.like.ws, _ = size()
	log.Printf("Telnet client %s connected to %s. WxH: %dx%d\r\n", c.LocalAddr(), c.RemoteAddr(), w.like.ws.Width, w.like.ws.Height)
	return w.like
}

// OptionCode returns the IAC code for NAWS.
func (l *likeSerialPort) OptionCode() byte {
	return telnet.TeloptNAWS
}

// Offer sends the IAC WILL NAWS command to the server.
func (l *likeSerialPort) Offer(c *telnet.Connection) {
	if l.console == nil {
		// Клиент.
		IAC(c, telnet.WILL, l.OptionCode())
	}
}

// HandleWill sends the IAC DO NAWS command to the client.
func (l *likeSerialPort) HandleWill(c *telnet.Connection) {
	handle(c, telnet.WILL, l.OptionCode())
	if l.console != nil {
		// Сервер.
		IAC(c, telnet.DO, l.OptionCode())
		// Не по RFC. Для dssh клиента.
		spam(c, COMPORT, SIGNATURE+SERVER, path)
	}
}

// HandleDo processes the monitor size options for NAWS.
func (l *likeSerialPort) HandleDo(c *telnet.Connection) {
	handle(c, telnet.DO, l.OptionCode())
	if l.console == nil {
		// Клиент.
		l.sizeTTY(c)
		go l.monitorSizeTTY(c)
	}
}

// HandleSB processes the information about window size sent from the client to the server.
func (l *likeSerialPort) HandleSB(c *telnet.Connection, b []byte) {
	if len(b) != 4 {
		return
	}
	if l.console != nil {
		// Сервер.
		l.ws.Width = binary.BigEndian.Uint16(b[0:2])
		l.ws.Height = binary.BigEndian.Uint16(b[2:4])
		log.Printf("%s<=%s IAC SB %v %s WxH: %dx%d\r\n", c.LocalAddr(), c.RemoteAddr(), b, cmdOpt(l.OptionCode()), l.ws.Width, l.ws.Height)
		l.console.SetSize(int(l.ws.Width), int(l.ws.Height))
	}
}

// Сравниваю size с l.ws и если отличаются то передаю на сервер.
func (l *likeSerialPort) monitorSizeTTY(c *telnet.Connection) {
	// Клиент с RFC1073
	// defer func() {
	// 	l.ws.Height = 0
	// 	l.ws.Width = 0
	// 	l.sizeTTY(c)
	// }()
	for {
		select {
		case <-ctx.Done():
			// log.Printf("monitorSizeTTY ctx.Done\r\n")
			return
		case <-time.After(time.Second):
			if l.closed {
				// log.Printf("monitorSizeTTY l.closed\r\n")
				return
			}
			ws, err := size()
			if err != nil {
				continue
			}
			if ws.Width != l.ws.Width || ws.Height != l.ws.Height {
				l.ws = ws
				l.sizeTTY(c)
			}
		}
	}
}

// Передаю серверу размер.
func (l *likeSerialPort) sizeTTY(c *telnet.Connection) {
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, l.OptionCode()})
	payload := new(bytes.Buffer)
	binary.Write(payload, binary.BigEndian, l.ws.Width)
	binary.Write(payload, binary.BigEndian, l.ws.Height)
	b.Write(escapeIAC(payload.Bytes()))
	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err := c.Conn.Write(b.Bytes())

	log.Printf("%s->%s %s %dx%d err:%v\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(l.OptionCode()), l.ws.Width, l.ws.Height, err)
}
