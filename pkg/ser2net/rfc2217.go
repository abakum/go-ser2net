package ser2net

// Telnet Com Port Control Option - https://tools.ietf.org/html/rfc2217

import (
	"bytes"
	"encoding/binary"
	"time"

	"github.com/PatrickRudolph/telnet"
	"go.bug.st/serial"
)

const (
	COMPORT byte = 0x2C
	SERVER  byte = 100
)
const (
	SIGNATURE byte = iota
	BAUDRATE
	DATASIZE
	PARITY
	STOPSIZE
	CONTROL
)

// Handler2217 negotiates Com Port for a specific connection.
type Handler2217 struct {
	old    serial.Mode
	worker *SerialWorker
	client bool
	will   bool
	do     bool
}

var H2217 *Handler2217

// Server2217 enables Com Port negotiation on a Server.
func Server2217(c *telnet.Connection) telnet.Negotiator {
	// log.Println("Server2217")
	go func() {
		// Мониторим изменения на сервере режима последовательной консоли
		first := true
		for {
			select {
			case <-H2217.worker.context.Done():
				return
			case <-time.After(time.Second):
				if first {
					first = false
					H2217.old = H2217.worker.mode
					continue
				}
				H2217.writeMode(c, &H2217.worker.mode)
			}
		}
	}()
	return H2217
}

// Client2217 enables Com Port negotiation on a Client.
func Client2217(c *telnet.Connection) telnet.Negotiator {
	// log.Println("Client2217")
	H2217.client = true
	return H2217
}

// OptionCode returns the IAC code for Com Port.
func (h *Handler2217) OptionCode() byte {
	return COMPORT
}

// Сервер пассивен
func (h *Handler2217) Offer(c *telnet.Connection) {
	if !h.client {
		return
	}
	// log.Println("Offer", h.client)
	c.Conn.Write([]byte{telnet.IAC, telnet.WILL, h.OptionCode()})
}

// Сервер ответит если к нему подключена последовательная консоль
func (h *Handler2217) HandleWill(c *telnet.Connection) {
	if h.client {
		return
	}
	// log.Println("HandleWill", h.client)
	if len(h.worker.args) == 0 {
		h.will = true
		c.Conn.Write([]byte{telnet.IAC, telnet.DO, h.OptionCode()})
		h.writeSignature(c)
	}
}

// Заменяем IAC на IAC IAC в writeSignature и writeBaudRate
func escapeIAC(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{telnet.IAC}, []byte{telnet.IAC, telnet.IAC})
}

// Немного о себе
func (h *Handler2217) writeSignature(c *telnet.Connection) {
	payload := new(bytes.Buffer)
	subopt := SIGNATURE
	if h.client {
		if !h.do {
			return
		}
		payload.WriteString(c.LocalAddr().String())
	} else {
		subopt += SERVER
		if !h.will {
			return
		}
		payload.WriteString(h.worker.path)
	}

	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt})
	b.Write(escapeIAC(payload.Bytes()))
	b.Write([]byte{telnet.IAC, telnet.SE})
	c.Conn.Write(b.Bytes())
}

// HandleDo send mode to server
func (h *Handler2217) HandleDo(c *telnet.Connection) {
	// log.Println("HandleDo", h.client, h.will)
	if !h.client {
		return
	}
	h.do = true
	h.writeBaudRate(c)
	h.writeDataBits(c)
	h.writeParity(c)
	h.writeStopBits(c)
	h.writeControll(c)
}

func (h *Handler2217) writeBaudRate(c *telnet.Connection) {
	subopt := BAUDRATE
	if h.client {
		if !h.do {
			return
		}
		h.writeSignature(c)
	} else {
		subopt += SERVER
		if !h.will {
			return
		}
	}
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt})
	// payload := make([]byte, 4)
	// binary.BigEndian.PutUint32(payload, uint32(h.will.BaudRate))
	payload := new(bytes.Buffer)
	binary.Write(payload, binary.BigEndian, uint32(h.old.BaudRate))
	b.Write(escapeIAC(payload.Bytes()))

	b.Write([]byte{telnet.IAC, telnet.SE})
	// log.Println("writeBaudRate", h.client, h.old.BaudRate, b.Bytes())
	c.Conn.Write(b.Bytes())

}
func (h *Handler2217) writeDataBits(c *telnet.Connection) {
	// log.Println("writeDataSize", h.client, h.will.DataBits)
	subopt := DATASIZE
	if h.client {
		if !h.do {
			return
		}
		h.writeSignature(c)
	} else {
		subopt += SERVER
		if !h.will {
			return
		}
	}
	c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		byte(h.old.DataBits),
		telnet.IAC, telnet.SE})
}
func (h *Handler2217) writeParity(c *telnet.Connection) {
	// log.Println("writeParity", h.client, h.will.Parity)
	subopt := PARITY
	if h.client {
		if !h.do {
			return
		}
		h.writeSignature(c)
	} else {
		subopt += SERVER
		if !h.will {
			return
		}
	}
	c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		byte(h.old.Parity) + 1,
		telnet.IAC, telnet.SE})
}
func (h *Handler2217) writeStopBits(c *telnet.Connection) {
	// log.Println("writeStopBits", h.client, h.will.StopBits)
	subopt := STOPSIZE
	if h.client {
		if !h.do {
			return
		}
		h.writeSignature(c)
	} else {
		subopt += SERVER
		if !h.will {
			return
		}
	}
	var b byte
	switch h.old.StopBits {
	case serial.OneStopBit:
		b = 1
	case serial.TwoStopBits:
		b = 2
	case serial.OnePointFiveStopBits:
		b = 3
	}
	c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		b,
		telnet.IAC, telnet.SE})
}
func (h *Handler2217) writeControll(c *telnet.Connection) {
	// log.Println("writeParity", h.client, h.will.Parity)
	subopt := CONTROL
	if h.client {
		if !h.do {
			return
		}
		h.writeSignature(c)
	} else {
		subopt += SERVER
		if !h.will {
			return
		}
	}
	c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		1,
		telnet.IAC, telnet.SE})
}

// HandleSB processes the information about Com Port sent from the client to the server and back.
func (h *Handler2217) HandleSB(c *telnet.Connection, b []byte) {
	if h.client {
		if !h.do {
			return
		}
		// log.Println("HandleSB", h.client, b, string(b))
	} else {
		if !h.will {
			return
		}
		// log.Println("\rHandleSB", h.client, b, string(b)+"\r")
	}
	subopt := b[0]
	val := b[1]
	switch subopt {
	case SIGNATURE, SIGNATURE + SERVER:
		s := string(b[1:])
		if h.client {
			if s == c.LocalAddr().String() {
				s = "RouterOS"
			}
			h.worker.url = s
		} else {
			h.worker.remote = s
		}
	case BAUDRATE, BAUDRATE + SERVER:
		h.old.BaudRate = int(binary.BigEndian.Uint32(b[1:]))
		if h.client {
			h.worker.mode.BaudRate = h.old.BaudRate
			return
		}
		if h.old.BaudRate > 0 {
			h.worker.SetMode(&h.old)
		} else {
			h.old = h.worker.mode
		}
		h.writeBaudRate(c)
	case DATASIZE, DATASIZE + SERVER:
		h.old.DataBits = int(val)
		if h.client {
			h.worker.mode.DataBits = h.old.DataBits
			return
		}
		if val > 0 {
			h.worker.SetMode(&h.old)
		} else {
			h.old = h.worker.mode
		}
		h.writeDataBits(c)
	case PARITY, PARITY + SERVER:
		h.old.Parity = serial.Parity(val - 1)
		if h.client {
			h.worker.mode.Parity = h.old.Parity
			return
		}
		if val > 0 {
			h.worker.SetMode(&h.old)
		} else {
			h.old = h.worker.mode
		}
		h.writeParity(c)
	case STOPSIZE, STOPSIZE + SERVER:
		switch val {
		case 1:
			h.old.StopBits = serial.OneStopBit
		case 2:
			h.old.StopBits = serial.TwoStopBits
		case 3:
			h.old.StopBits = serial.OnePointFiveStopBits
		}
		if h.client {
			h.worker.mode.StopBits = h.old.StopBits
			return
		}
		if val > 0 {
			h.worker.SetMode(&h.old)
		} else {
			h.old = h.worker.mode
		}
		h.writeStopBits(c)
	}
}

// Если на сервере поменяли режим последовательной консоли то сообщаем клиентам
func (h *Handler2217) writeMode(c *telnet.Connection, mode *serial.Mode) {
	if h.old.BaudRate != mode.BaudRate {
		h.old.BaudRate = mode.BaudRate
		h.writeBaudRate(c)
	}
	if h.old.DataBits != mode.DataBits {
		h.old.DataBits = mode.DataBits
		h.writeDataBits(c)
	}
	if h.old.Parity != mode.Parity {
		h.old.Parity = mode.Parity
		h.writeParity(c)
	}
	if h.old.StopBits != mode.StopBits {
		h.old.StopBits = mode.StopBits
		h.writeStopBits(c)
	}
}
