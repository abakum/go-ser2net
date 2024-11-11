package ser2net

// Telnet Com Port Control Option - https://tools.ietf.org/html/rfc2217

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
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

// Клиент.
type Client struct {
	c      *telnet.Connection
	other  serial.Mode
	enable bool
	last   time.Time
}

// Переключает клиента.
func (w *SerialWorker) setEnable(c *telnet.Connection, b bool) {
	cl := w.get(c)
	if cl.enable == b {
		return
	}
	cl.enable = b
	w.set(c, cl)
}

// Включен ли клиент.
func (w *SerialWorker) enable(c *telnet.Connection) bool {
	return w.get(c).enable
}

// Возвращает данные клиента и создаёт его если не было
func (w *SerialWorker) get(c *telnet.Connection) (cl Client) {
	s := c.RemoteAddr().String()
	cl, ok := w.cls[s]
	if ok {
		return
	}
	cl = Client{
		other: w.mode,
		c:     c,
	}
	w.set(c, cl)
	return
}

// Обновляет или добавляет данные о клиенте
func (w *SerialWorker) set(c *telnet.Connection, cl Client) {
	s := c.RemoteAddr().String()
	cl.last = time.Now()
	w.clm.Lock()
	w.cls[s] = cl
	w.clm.Unlock()
}

// Удаляет клиента
func (w *SerialWorker) meDel(c *telnet.Connection) {
	s := c.RemoteAddr().String()
	w.clm.Lock()
	delete(w.cls, s)
	w.clm.Unlock()
}

// Server2217 enables Com Port negotiation on a Server.
// Добавляет клиента в h.cons при подключении.
// Убирает клиента из h.cons когда соединение с ним прекращается.
func (w *SerialWorker) Server2217(c *telnet.Connection) telnet.Negotiator {
	if len(w.args) > 0 {
		return w
	}
	log.Printf("RFC2217 telnet server %s accepted connection from %s. Mode: %v\r\n", c.LocalAddr(), c.RemoteAddr(), w.mode)
	go func() {
		defer func() {
			w.meDel(c)
			log.Printf("RFC2217 telnet client from %s is disconnected\r\n", c.RemoteAddr())
		}()
		for {
			if w == nil {
				return
			}
			select {
			case <-w.context.Done():
				w.iac(c, telnet.DONT, w.OptionCode())
				return
			case <-time.After(time.Second * 60):
				cl := w.get(c)
				if time.Since(cl.last) < time.Second*59 {
					continue
				}
				w.set(c, cl)
				if w.Controll(c) != nil {
					return
				}
			}
		}
	}()
	return w
}

// Client2217 enables Com Port negotiation on a Client.
func (w *SerialWorker) Client2217(c *telnet.Connection) telnet.Negotiator {
	if w.rfc2217 != nil {
		// Для клиента RFC2217 на сервере свой экземпляр SerialWorker.
		w, _ = NewSerialWorker(w.context, "", 0)
	} else {
		// Чтоб запросить режим
		w.mode = serial.Mode{}
	}
	log.Printf("RFC2217 telnet client %s connected to %s. Mode: %v\r\n", c.LocalAddr(), c.RemoteAddr(), w.get(c).other)
	return w
}

// OptionCode returns the IAC code for Com Port.
func (*SerialWorker) OptionCode() byte {
	return COMPORT
}

// Пишет iac и bs если без ошибки то клиент активен.
func (w *SerialWorker) iac(c *telnet.Connection, bs ...byte) (err error) {
	b := new(bytes.Buffer)
	b.WriteByte(telnet.IAC)
	b.Write(bs)
	_, err = c.Conn.Write(b.Bytes())
	w.setEnable(c, err == nil)
	log.Printf("%s->%s IAC %v %v\r\n", c.LocalAddr(), c.RemoteAddr(), bs, err)
	return
}

// Сервер пассивен.
func (w *SerialWorker) Offer(c *telnet.Connection) {
	if w.rfc2217 != nil {
		// Сервер не инициирует обмен
		return
	}
	w.iac(c, telnet.WILL, w.OptionCode())
}

// Сервер ответит если к нему подключена последовательная консоль
func (w *SerialWorker) HandleWill(c *telnet.Connection) {
	if w.rfc2217 == nil {
		// Если клиент
		return
	}
	if len(w.args) > 0 {
		// Если интерпретатор команд или команда то не DO
		w.iac(c, telnet.DONT, w.OptionCode())
		w.setEnable(c, true)
		w.signature(c)
		w.setEnable(c, false)
		return
	}
	w.iac(c, telnet.DO, w.OptionCode())
	w.signature(c)
}

// Заменяем IAC на IAC IAC в writeSignature и writeBaudRate
func escapeIAC(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{telnet.IAC}, []byte{telnet.IAC, telnet.IAC})
}

// Немного о себе
func (w *SerialWorker) signature(c *telnet.Connection) (err error) {
	if !w.enable(c) {
		return
	}
	payload := new(bytes.Buffer)
	subopt := SIGNATURE
	s := w.path
	if w.rfc2217 == nil {
		s = c.LocalAddr().String()
	} else {
		subopt += SERVER
	}
	payload.WriteString(s)

	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt})
	b.Write(escapeIAC(payload.Bytes()))
	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err = c.Conn.Write(b.Bytes())
	w.setEnable(c, err == nil)
	log.Printf("%s Signature %s to %s %v\r\n", c.LocalAddr(), s, c.RemoteAddr(), err)
	return
}

// Запрашиваем режим у сервера
func (w *SerialWorker) HandleDo(c *telnet.Connection) {
	if w.rfc2217 != nil {
		// Сервер не инициирует обмен
		return
	}
	w.setEnable(c, true)
	w.baudRate(c) // До первого Controll запрос baudRate
	w.dataBits(c)
	w.parity(c)
	w.stopBits(c)
	w.Controll(c)
}

// Для нового клиента запрашиваем значение.
// Иначе устанавливаем w.mode.BaudRate.
func (w *SerialWorker) baudRate(c *telnet.Connection) (err error) {
	if !w.enable(c) {
		return
	}
	subopt := BAUDRATE
	v := w.mode.BaudRate
	if w.rfc2217 == nil {
		// Клиент
		if w.mode.InitialStatusBits != nil {
			// Если скорость не запрашивается то представляемся
			if w.signature(c) != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}

	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt})
	payload := new(bytes.Buffer)
	binary.Write(payload, binary.BigEndian, uint32(v))
	b.Write(escapeIAC(payload.Bytes()))

	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err = c.Conn.Write(b.Bytes())
	w.setEnable(c, err == nil)
	log.Printf("%s BaudRate %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	return
}

// Для нового клиента запрашиваем значение.
// Иначе устанавливаем w.mode.dataBits.
func (w *SerialWorker) dataBits(c *telnet.Connection) (err error) {
	if !w.enable(c) {
		return
	}
	subopt := DATASIZE
	v := w.mode.DataBits
	if w.rfc2217 == nil {
		// Клиент
		if w.mode.InitialStatusBits != nil {
			// Если скорость не запрашивается то представляемся
			if w.signature(c) != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}

	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	w.setEnable(c, err == nil)
	log.Printf("%s DataBits %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	return
}

func (w *SerialWorker) parity(c *telnet.Connection) (err error) {
	if !w.enable(c) {
		return
	}
	subopt := PARITY
	v := w.mode.Parity
	switch v {
	case serial.NoParity:
		v = 1
	case serial.OddParity:
		v = 2
	case serial.EvenParity:
		v = 3
	case serial.MarkParity:
		v = 4
	case serial.SpaceParity:
		v = 5
	}
	if w.rfc2217 == nil {
		if w.mode.InitialStatusBits != nil {
			// Если скорость не запрашивается то представляемся
			if w.signature(c) != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}
	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	w.setEnable(c, err == nil)
	log.Printf("%s Parity %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	return
}

func (w *SerialWorker) stopBits(c *telnet.Connection) (err error) {
	if !w.enable(c) {
		return
	}
	subopt := STOPSIZE
	v := w.mode.StopBits
	switch v {
	case serial.OneStopBit:
		v = 1
	case serial.TwoStopBits:
		v = 2
	case serial.OnePointFiveStopBits:
		v = 3
	}
	if w.rfc2217 == nil {
		if w.mode.InitialStatusBits != nil {
			// Если скорость не запрашивается то представляемся
			if w.signature(c) != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}
	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	w.setEnable(c, err == nil)
	log.Printf("%s StopBits %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	return
}

// Принудительно flowControl=N и единожды writeSignature для клиента.
// Используем для проверки живости.
func (w *SerialWorker) Controll(c *telnet.Connection) (err error) {
	cl := w.get(c)
	if !cl.enable {
		return
	}
	subopt := CONTROL
	v := 1 // N
	if w.rfc2217 == nil {
		if w.mode.InitialStatusBits == nil {
			w.mode.InitialStatusBits = &serial.ModemOutputBits{}
			w.set(c, cl)
			if w.signature(c) != nil {
				return
			}
		}
	} else {
		subopt += SERVER
	}
	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	w.setEnable(c, err == nil)
	log.Printf("%s Controll %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	return
}

// HandleSB processes the information about Com Port sent from the client to the server and back.
// Вызывается из read
func (w *SerialWorker) HandleSB(c *telnet.Connection, b []byte) {
	cl := w.get(c)
	if !cl.enable {
		return
	}
	info := fmt.Sprintf("%s->%s IAC SB %v", c.RemoteAddr(), c.LocalAddr(), b)

	subopt := b[0]
	v := int(b[1])

	switch subopt {
	case SIGNATURE, SIGNATURE + SERVER:
		s := string(b[1:])
		if w.rfc2217 == nil {
			if s == c.LocalAddr().String() {
				s = "RouterOS"
			}
			if w.url == "" {
				w.url = s
			}
		} else {
			if w.remote == s {
				return
			}
			// У сервера клиентов много.
			w.clm.Lock()
			w.remote = s
			w.clm.Unlock()
		}

		info += " signature: " + s
		log.Print(info, "\r\n")
	case BAUDRATE, BAUDRATE + SERVER:
		v = int(binary.BigEndian.Uint32(b[1:]))
		info += " baudRate: "
		if v > 0 {
			info += fmt.Sprintf("%d", v)
		} else {
			info += "?"
		}
		log.Print(info, "\r\n")
		cl.other.BaudRate = v
		if w.rfc2217 == nil {
			w.set(c, cl)
			w.mode.BaudRate = cl.other.BaudRate
			return
		}
		// Сервер
		if v > 0 {
			w.SetMode(&cl.other)
			return
		}
		// Ответ на запрос о скорости
		w.baudRate(c)
	case DATASIZE, DATASIZE + SERVER:
		info += " dataBits: "
		if v > 0 {
			info += fmt.Sprintf("%d", v)
		} else {
			info += "?"
		}
		log.Print(info, "\r\n")
		cl.other.DataBits = v
		if w.rfc2217 == nil {
			w.set(c, cl)
			w.mode.DataBits = cl.other.DataBits
			return
		}
		if v > 0 {
			w.SetMode(&cl.other)
			return
		}
		w.dataBits(c)
	case PARITY, PARITY + SERVER:
		info += " parity: "
		switch v {
		case 1:
			cl.other.Parity = serial.NoParity
			info += "N"
		case 2:
			cl.other.Parity = serial.OddParity
			info += "O"
		case 3:
			cl.other.Parity = serial.EvenParity
			info += "E"
		case 4:
			cl.other.Parity = serial.MarkParity
			info += "M"
		case 5:
			cl.other.Parity = serial.SpaceParity
			info += "S"
		default:
			info += "?"
		}
		log.Print(info, "\r\n")
		if w.rfc2217 == nil {
			w.set(c, cl)
			w.mode.Parity = cl.other.Parity
			return
		}
		if v > 0 {
			w.SetMode(&cl.other)
			return
		}
		w.parity(c)
	case STOPSIZE, STOPSIZE + SERVER:
		info += " stopBits: "
		switch v {
		case 1:
			cl.other.StopBits = serial.OneStopBit
			info += "1"
		case 2:
			cl.other.StopBits = serial.TwoStopBits
			info += "2"
		case 3:
			cl.other.StopBits = serial.OnePointFiveStopBits
			info += "1.5"
		default:
			info += "?"
		}
		log.Print(info, "\r\n")
		if w.rfc2217 == nil {
			w.set(c, cl)
			w.mode.StopBits = cl.other.StopBits
			return
		}
		if v > 0 {
			w.SetMode(&cl.other)
			return
		}
		w.stopBits(c)
	case CONTROL, CONTROL + SERVER:
		info += " flowControl: N"
	}
}
