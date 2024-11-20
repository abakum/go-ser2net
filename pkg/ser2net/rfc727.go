package ser2net

// https://datatracker.ietf.org/doc/html/rfc727

import (
	"github.com/PatrickRudolph/telnet"
)

type Logout struct {
	w      *SerialWorker
	client bool
}

func (w *SerialWorker) Server727(c *telnet.Connection) telnet.Negotiator {
	l := &Logout{w, false}
	// log.Printf("%s server %s accepted connection from %s.\r\n", cmdOpt(l.OptionCode()), c.LocalAddr(), c.RemoteAddr())
	return l
}

func (w *SerialWorker) Client727(c *telnet.Connection) telnet.Negotiator {
	l := &Logout{w, true}
	// log.Printf("%s client %s connected to %s\r\n", cmdOpt(l.OptionCode()), c.LocalAddr(), c.RemoteAddr())
	if w.rfc2217 != nil {
		// Для клиента RFC727 на сервере свой экземпляр SerialWorker.
		l.w, _ = NewSerialWorker(l.w.context, "", 0)
	}
	return l
}

func (*Logout) OptionCode() byte {
	return telnet.TeloptLOGOUT
}

func (*Logout) Offer(c *telnet.Connection) {
}

func (l *Logout) HandleWill(c *telnet.Connection) {
	handle(c, telnet.WILL, l.OptionCode())
	if l.client {
		// Клиент.
		// Если этот WILL от alive то сообщаем о живости.
		// Иначе это подтверждение намерения клиента выйти и если сервер закрыл соединения то и клиент его закрывает.
		if IAC(c, telnet.DONT, l.OptionCode()) != nil {
			c.Close()
		}
	}
}

func (l *Logout) HandleDo(c *telnet.Connection) {
	handle(c, telnet.DO, l.OptionCode())
	if l.client {
		// Клиент.
		// По RFC сервер не шлёт DO перед отключением.
		c.Close()
		return
	}
	// Сервер.
	IAC(c, telnet.WILL, telnet.TeloptLOGOUT)
	// log.Printf("%s client %s logout\r\n", cmdOpt(l.OptionCode()), c.RemoteAddr())
	if l.w.exist(c) {
		l.w.get(c).done <- true
	}
	c.Close()
}

func (*Logout) HandleSB(c *telnet.Connection, b []byte) {
}
