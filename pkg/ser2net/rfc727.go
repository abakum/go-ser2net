package ser2net

// https://datatracker.ietf.org/doc/html/rfc727
// Для alive

import (
	"log"

	"github.com/PatrickRudolph/telnet"
)

type Logout struct {
	w    *SerialWorker
	done chan bool
}

func (w *SerialWorker) Server727(c *telnet.Connection) telnet.Negotiator {
	log.Printf("Telnet server %s accepted connection from %s.\r\n", c.LocalAddr(), c.RemoteAddr())
	done := make(chan bool)
	go func() {
		select {
		case <-done:
			// log.Printf("Server727 %s done\r\n", c.RemoteAddr())
			log.Printf("Telnet client %s logout\r\n", c.RemoteAddr())
		case <-w.context.Done():
			// log.Printf("Server727 %s Done\r\n", c.RemoteAddr())
			// По RFC сервер не шлёт DO перед отключением.
			IAC(c, telnet.DO, telnet.TeloptLOGOUT)
			log.Printf("Telnet client %s is disconnected by server\r\n", c.RemoteAddr())
		}
		w.del(c)
		c.Close()
	}()
	return &Logout{w, done}
}

func (w *SerialWorker) Client727(c *telnet.Connection) telnet.Negotiator {
	log.Printf("Telnet client %s connected to %s\r\n", c.LocalAddr(), c.RemoteAddr())
	l := &Logout{w, nil}
	if l.w.rfc2217 != nil {
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
	if l.done == nil {
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
	if l.done == nil {
		// Клиент.
		// По RFC сервер не шлёт DO перед отключением.
		c.Close()
		return
	}
	// Сервер.
	IAC(c, telnet.WILL, telnet.TeloptLOGOUT)
	l.done <- true
}

func (*Logout) HandleSB(c *telnet.Connection, b []byte) {
}
