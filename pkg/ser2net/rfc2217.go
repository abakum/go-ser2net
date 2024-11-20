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
	COMPORT    byte = 0x2C
	SERVER     byte = 100
	ALIVE           = time.Minute
	MUTELOGOUT      = true
	// ALIVE      = time.Second * 15 // Тест.
	// MUTELOGOUT = false
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
	remote serial.Mode
	last   time.Time
	done   chan bool
}

// Server2217 enables Com Port negotiation on a Server.
func (w *SerialWorker) Server2217(c *telnet.Connection) telnet.Negotiator {
	// log.Printf("%s server %s accepted connection from %s. Mode: %v\r\n", cmdOpt(w.OptionCode()), c.LocalAddr(), c.RemoteAddr(), w.mode)
	c.SetWindowTitle(w.String())
	return w
}

// Client2217 enables Com Port negotiation on a Client.
// Добавляет клиента в w.cls при подключении через w.get(c).remote.
func (w *SerialWorker) Client2217(c *telnet.Connection) telnet.Negotiator {

	if w.rfc2217 != nil {
		// Для клиента RFC2217 на сервере свой экземпляр SerialWorker.
		w, _ = NewSerialWorker(w.context, "", 0)
	} else {
		// Чтоб запросить режим.
		w.mode = serial.Mode{}
	}
	// log.Printf("%s client %s connected to %s. Mode: %v\r\n", cmdOpt(w.OptionCode()), c.LocalAddr(), c.RemoteAddr(), w.get(c).remote)
	w.get(c) // Добавляем клиента в список
	return w
}

// OptionCode returns the IAC code for Com Port.
func (*SerialWorker) OptionCode() byte {
	return COMPORT
}

// Сервер пассивен.
func (w *SerialWorker) Offer(c *telnet.Connection) {
	if w.rfc2217 == nil {
		// Клиент.
		w.iac(c, telnet.WILL, w.OptionCode())
	}
}

// Сервер шлёт подпись и мониторит отключени клиентов.
func (w *SerialWorker) HandleWill(c *telnet.Connection) {
	handle(c, telnet.WILL, w.OptionCode())
	if w.rfc2217 == nil ||
		w.exist(c) {
		// Клиент.
		// Защита от второго WILL.
		return
	}
	cl := w.get(c) // Как сервер представляет клиента.
	cl.done = make(chan bool)
	// w.set(c, cl)
	w.iac(c, telnet.DO, w.OptionCode())
	w.signature(c)
	go func() {
		defer func() {
			w.del(c)
			c.Close()
		}()
		for {
			select {
			case <-cl.done:
				log.Printf("%s client %s logout\r\n", cmdOpt(w.OptionCode()), c.RemoteAddr())
				return
			case <-w.context.Done():
				log.Printf("%s client %s is disconnected by server\r\n", cmdOpt(w.OptionCode()), c.RemoteAddr())
				return
			case <-time.After(ALIVE):
				if !w.exist(c) {
					return
				}
				cl := w.get(c)
				if time.Since(cl.last) < ALIVE-time.Second {
					continue
				}
				// Живой?
				if IAC(c, telnet.WILL, telnet.TeloptLOGOUT) != nil {
					return
				}
			}
		}
	}()
}

// Клиент запрашивает режим у сервера.
func (w *SerialWorker) HandleDo(c *telnet.Connection) {
	handle(c, telnet.DO, w.OptionCode())
	if w.rfc2217 != nil {
		// Сервер не инициирует обмен
		return
	}
	// Без представления.
	w.baudRate(c)
	w.dataBits(c)
	w.parity(c)
	w.stopBits(c)
	// Представляемся
	w.control(c)
}

// HandleSB processes the information about Com Port sent from the client to the server and back.
// Вызывается из read.
func (w *SerialWorker) HandleSB(c *telnet.Connection, b []byte) {
	if len(b) < 2 || !w.exist(c) {
		// Спам.
		// Не живой клиент.
		return
	}
	cl := w.get(c)

	client := w.rfc2217 == nil
	subopt := b[0]
	v := int(b[1])

	info := fmt.Sprintf("%s<=%s IAC SB %v %s ", c.LocalAddr(), c.RemoteAddr(), b, cmdOpt(subopt))

	switch subopt {
	case SIGNATURE, SIGNATURE + SERVER:
		s := string(b[1:])
		if client {
			// RouterOS возвращает подпись клиента.
			// ser2net представляется как ser2net.
			if s == c.LocalAddr().String() {
				s = "RouterOS"
			}
			// Запомним первое представление.
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

		info += s
		log.Print(info, "\r\n")
	case BAUDRATE, BAUDRATE + SERVER:
		v = int(binary.BigEndian.Uint32(b[1:]))
		if v > 0 {
			info += fmt.Sprintf("%d", v)
		} else {
			info += "?"
		}
		log.Print(info, "\r\n")
		cl.remote.BaudRate = v
		if client {
			// w.set(c, cl)
			w.mode.BaudRate = cl.remote.BaudRate
			return
		}
		// Сервер
		if v > 0 {
			w.SetMode(&cl.remote)
			return
		}
		// Ответ на запрос о скорости
		w.baudRate(c)
	case DATASIZE, DATASIZE + SERVER:
		if v > 0 {
			info += fmt.Sprintf("%d", v)
		} else {
			info += "?"
		}
		log.Print(info, "\r\n")
		cl.remote.DataBits = v
		if client {
			// w.set(c, cl)
			w.mode.DataBits = cl.remote.DataBits
			return
		}
		if v > 0 {
			w.SetMode(&cl.remote)
			return
		}
		w.dataBits(c)
	case PARITY, PARITY + SERVER:
		switch v {
		case 1:
			cl.remote.Parity = serial.NoParity
			info += "N"
		case 2:
			cl.remote.Parity = serial.OddParity
			info += "O"
		case 3:
			cl.remote.Parity = serial.EvenParity
			info += "E"
		case 4:
			cl.remote.Parity = serial.MarkParity
			info += "M"
		case 5:
			cl.remote.Parity = serial.SpaceParity
			info += "S"
		default:
			info += "?"
		}
		log.Print(info, "\r\n")
		if client {
			// w.set(c, cl)
			w.mode.Parity = cl.remote.Parity
			return
		}
		if v > 0 {
			w.SetMode(&cl.remote)
			return
		}
		w.parity(c)
	case STOPSIZE, STOPSIZE + SERVER:
		switch v {
		case 1:
			cl.remote.StopBits = serial.OneStopBit
			info += "1"
		case 2:
			cl.remote.StopBits = serial.TwoStopBits
			info += "2"
		case 3:
			cl.remote.StopBits = serial.OnePointFiveStopBits
			info += "1.5"
		default:
			info += "?"
		}
		log.Print(info, "\r\n")
		if client {
			// w.set(c, cl)
			w.mode.StopBits = cl.remote.StopBits
			return
		}
		if v > 0 {
			w.SetMode(&cl.remote)
			return
		}
		w.stopBits(c)
	case CONTROL, CONTROL + SERVER:
		info += "N"
		log.Print(info, "\r\n")
		if !client {
			w.control(c)
		}
	}
}

// Для нового клиента запрашиваем значение.
// Иначе устанавливаем w.mode.BaudRate.
func (w *SerialWorker) baudRate(c *telnet.Connection) (err error) {
	if !w.exist(c) {
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
	log.Printf("%s->%s %s %d\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(subopt), v)
	_, err = c.Conn.Write(b.Bytes())
	if err != nil {
		w.get(c).done <- true
	}
	return
}

// Для нового клиента запрашиваем значение.
// Иначе устанавливаем w.mode.dataBits.
func (w *SerialWorker) dataBits(c *telnet.Connection) (err error) {
	if !w.exist(c) {
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

	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	log.Printf("%s->%s %s %d\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(subopt), v)
	_, err = c.Conn.Write(b.Bytes())
	if err != nil {
		w.get(c).done <- true
	}
	return
}

func (w *SerialWorker) parity(c *telnet.Connection) (err error) {
	if !w.exist(c) {
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

	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	log.Printf("%s->%s %s %d\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(subopt), v)
	_, err = c.Conn.Write(b.Bytes())
	if err != nil {
		w.get(c).done <- true
	}
	return
}

func (w *SerialWorker) stopBits(c *telnet.Connection) (err error) {
	if !w.exist(c) {
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
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	log.Printf("%s->%s %s %d\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(subopt), v)
	_, err = c.Conn.Write(b.Bytes())
	if err != nil {
		w.get(c).done <- true
	}
	return
}

// Принудительно flowControl=N и единожды writeSignature для клиента.
func (w *SerialWorker) control(c *telnet.Connection) (err error) {
	if !w.exist(c) {
		return
	}
	// cl := w.get(c)
	subopt := CONTROL
	v := 1 // N
	if w.rfc2217 == nil {
		if w.mode.InitialStatusBits == nil {
			// Чтоб представлялись при смене режима.
			w.mode.InitialStatusBits = &serial.ModemOutputBits{}
			// w.set(c, cl)
			if w.signature(c) != nil {
				return
			}
		}
	} else {
		subopt += SERVER
	}
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, w.OptionCode(), subopt,
		byte(v),
		telnet.IAC, telnet.SE})
	log.Printf("%s->%s %s %d\r\n", c.LocalAddr(), c.RemoteAddr(), cmdOpt(subopt), v)
	_, err = c.Conn.Write(b.Bytes())
	if err != nil {
		w.get(c).done <- true
	}
	return
}

// Клиент в списке.
func (w *SerialWorker) exist(c *telnet.Connection) (ok bool) {
	_, ok = w.cls[c.RemoteAddr().String()]
	return
}

// Возвращает данные клиента и создаёт его если не было.
func (w *SerialWorker) get(c *telnet.Connection) (cl *Client) {
	s := c.RemoteAddr().String()
	cl, ok := w.cls[s]
	if ok {
		return
	}
	cl = &Client{
		remote: w.mode,
		c:      c,
	}
	w.clm.Lock()
	w.cls[s] = cl
	w.clm.Unlock()
	// w.set(c, cl)
	return
}

// Обновляет или добавляет данные о клиенте.
// func (w *SerialWorker) set(c *telnet.Connection, cl Client) {
// 	s := c.RemoteAddr().String()
// 	cl.last = time.Now()
// 	w.clm.Lock()
// 	w.cls[s] = cl
// 	w.clm.Unlock()
// }

// Удаляет клиента
func (w *SerialWorker) del(c *telnet.Connection) {
	s := c.RemoteAddr().String()
	w.clm.Lock()
	delete(w.cls, s)
	w.clm.Unlock()
}

// Для протокола.
func cmdOpt(b byte) string {
	switch b {
	case telnet.WILL:
		return "WILL"
	case telnet.WONT:
		return "WONT"
	case telnet.DO:
		return "DO"
	case telnet.DONT:
		return "DONT"
	case COMPORT:
		return "ComPort"
	case telnet.TeloptLOGOUT:
		return "LogOut"
	case telnet.TeloptNAWS:
		return "NAWS"
	case SIGNATURE, SIGNATURE + SERVER:
		return "Signature"
	case BAUDRATE, BAUDRATE + SERVER:
		return "BaudRate"
	case DATASIZE, DATASIZE + SERVER:
		return "DataBits"
	case PARITY, PARITY + SERVER:
		return "Parity"
	case STOPSIZE, STOPSIZE + SERVER:
		return "StopBits"
	case CONTROL, CONTROL + SERVER:
		return "FlowControl"
	}
	return "?"
}

// Посылаем IAC и bs.
func IAC(c *telnet.Connection, v ...byte) (err error) {
	_, err = c.Conn.Write(iac(c, v...))
	// log.Printf("%s->%s IAC err: %v\r\n", c.LocalAddr(), c.RemoteAddr(), err)
	return
}

// Протокол посылки данных.
func iac(c *telnet.Connection, v ...byte) []byte {
	b := new(bytes.Buffer)
	b.WriteByte(telnet.IAC)
	if len(v) > 0 {
		b.Write(v)
	}
	opt := ""
	if len(v) > 1 {
		opt = cmdOpt(v[1])
		if MUTELOGOUT && v[1] == telnet.TeloptLOGOUT {
			return b.Bytes()
		}
	}
	log.Printf("%s->%s IAC %v %s %s\r\n", c.LocalAddr(), c.RemoteAddr(), v, cmdOpt(v[0]), opt)
	return b.Bytes()
}

// Протокол получения данных.
func handle(c *telnet.Connection, v ...byte) {
	opt := ""
	if len(v) > 1 {
		opt = cmdOpt(v[1])
		if MUTELOGOUT && v[1] == telnet.TeloptLOGOUT {
			return
		}
	}
	log.Printf("%s<=%s IAC %v %s %s\r\n", c.LocalAddr(), c.RemoteAddr(), v, cmdOpt(v[0]), opt)
}

// Пишет iac и bs если ошибка то клиента отключаем.
func (w *SerialWorker) iac(c *telnet.Connection, v ...byte) (err error) {
	err = IAC(c, v...)
	if err != nil {
		w.get(c).done <- true
	}
	return
}

// Заменяем IAC на IAC IAC.
func escapeIAC(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{telnet.IAC}, []byte{telnet.IAC, telnet.IAC})
}

// Используем не по RFC для -Hcmd -Hbash.
func spam(c *telnet.Connection, opt, subopt byte, v string) (err error) {
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, opt, subopt})
	if len(v) > 0 {
		payload := new(bytes.Buffer)
		payload.WriteString(v)
		b.Write(escapeIAC(payload.Bytes()))
	}
	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err = c.Conn.Write(b.Bytes())
	if len(v) > 0 {
		log.Printf("%s->%s Signature %v\r\n", c.LocalAddr(), c.RemoteAddr(), v)
	}
	return
}

// Немного о себе.
func (w *SerialWorker) signature(c *telnet.Connection) (err error) {
	if !w.exist(c) {
		return
	}
	subopt := SIGNATURE
	v := w.path
	if w.rfc2217 == nil {
		// Клиент.
		v = c.LocalAddr().String()
	} else {
		subopt += SERVER
	}

	err = spam(c, w.OptionCode(), subopt, v)
	if err != nil {
		w.get(c).done <- true
	}
	return
}
