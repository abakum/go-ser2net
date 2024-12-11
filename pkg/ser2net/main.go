package ser2net

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PatrickRudolph/telnet"
	"github.com/PatrickRudolph/telnet/options"
	"github.com/abakum/go-console"
	"github.com/sorenisanerd/gotty/server"
	"github.com/sorenisanerd/gotty/utils"
	"go.bug.st/serial"
)

const (
	b16 = 16
	k1  = 1024
	k4  = 4 * k1
	k32 = 32 * k1
	lh  = "127.0.0.1"
)

// SerialWorker instances one serial-network bridge
type SerialWorker struct {
	// serial connection
	serialConn serial.Port
	// serial port settings
	mode serial.Mode
	// serial port path
	path string
	// is connected
	connected bool
	// Mutex for rx handling
	mux sync.Mutex

	lastErr    string
	txJobQueue chan byte
	rxJobQueue []chan byte

	context  context.Context
	cancel   context.CancelFunc
	web      *server.Server // -88
	url      string
	quitting bool
	// -H:2322 -Hcmd
	like *likeSerialPort
	// -Hcmd
	args []string
	pid  int
	in   *Stdin
	// -22
	rfc2217 *telnet.Server
	cls     map[string]*Client
	clm     sync.Mutex // Для cls
	remote  string
}

func SerialClose(port serial.Port) error {
	if port == nil {
		return nil
	}
	port.ResetInputBuffer()
	port.ResetOutputBuffer()
	port.Drain()
	return port.Close()
}

type Mode struct {
	serial.Mode
	path string
}

type SerialPort struct {
	serial.Port
	Mode
}

// Open opens the serial port using the specified modes
func Open(portName string, mode *serial.Mode) (*SerialPort, error) {
	port, err := serial.Open(portName, mode)
	if err != nil {
		// Return a nil interface, for which var==nil is true (instead of
		// a nil pointer to a struct that satisfies the interface).
		return nil, err
	}
	return &SerialPort{port, Mode{*mode, portName}}, err
}

// Некоторые устройства имеют короткий буфер и медленно из него читают.
// Будем передавать по одному байту за раз.
func (w *SerialPort) Write1(p []byte) (n int, err error) {
	for i, b := range p {
		_, err = w.Port.Write([]byte{b})
		if err != nil {
			return i, err
		}
	}
	return len(p), nil
}

// Имя порта типа com3 или /dev/ttyUSB0
func (w *SerialPort) String() string {
	return w.path
}

func (w *SerialPort) SerialClose() error {
	return SerialClose(w.Port)
}

func (m Mode) String() string {
	p := "N"
	switch m.Parity {
	case serial.OddParity:
		p = "O"
	case serial.EvenParity:
		p = "E"
	case serial.MarkParity:
		p = "M"
	case serial.SpaceParity:
		p = "S"
	}
	s := "1"
	switch m.StopBits {
	case serial.OnePointFiveStopBits:
		s = "1.5"
	case serial.TwoStopBits:
		s = "2"
	}
	path := ""
	if m.path != "" {
		path += m.path + "@"
	}
	return fmt.Sprintf("%s%d,%d,%s,%s",
		path, m.BaudRate, m.DataBits, p, s)
}

func (w *SerialWorker) String() string {
	connected := "connected"
	if !w.connected {
		connected = "not " + connected
	}
	if w.url != "" {
		connected += " to " + w.url
	}
	path := ""
	if w.path != "" {
		path += w.path + "@"
	}
	if w.like != nil {
		if len(w.args) > 0 {
			if w.pid > 0 {
				return fmt.Sprintf("$%s%d",
					path, w.pid)
			}
			return fmt.Sprintf("$%s %s",
				path, connected)
		}
		telnet := fmt.Sprintf("telnet://%s %s",
			w.path, connected)
		if strings.Contains(connected, "$") {
			return telnet
		}
		if strings.Contains(w.path, ":") {
			return fmt.Sprintf("%s@%s",
				telnet, Mode{w.mode, ""})
		}
	}
	if w.remote != "" {
		connected += " from " + w.remote
	}
	return fmt.Sprintf("%s %s",
		Mode{w.mode, w.path}, connected)
}

// Останавливает сервер w.rfc2217.
// Останавливает сервер w.web через w.cancel().
// Останавливает последовательный порт w.cancel().
func (w *SerialWorker) Stop() {
	// log.Printf("SerialWorker.Stop %s %v %v %v\r\n", w.url, w.connected, w.cancel, w.rfc2217)

	w.url = ""
	if w.cancel != nil {
		w.cancel()
	}
	if w.rfc2217 != nil {
		w.rfc2217.Stop()
	}
}

// Сервер меняет режим консоли и рассылает об этом сообщения клиентам.
// Клиент посылает сообщение серверу.
func (w *SerialWorker) SetMode(mode *serial.Mode) (err error) {
	if len(w.args) > 0 {
		// Не сериал
		return
	}
	err = w.serialConn.SetMode(mode)
	// log.Printf("SetMode %#v\r\n%#v %v\r\n", *mode, w.cls, err)
	if err != nil {
		return
	}
	w.mode = *mode
	// Рассылает всем изменения mode.
	for _, cl := range w.cls {
		if cl.remote.BaudRate != mode.BaudRate {
			if w.baudRate(cl.c) == nil {
				cl.remote.BaudRate = mode.BaudRate
			}
		}
		if cl.remote.DataBits != mode.DataBits {
			if w.dataBits(cl.c) == nil {
				cl.remote.DataBits = mode.DataBits
			}
		}
		if cl.remote.Parity != mode.Parity {
			if w.parity(cl.c) == nil {
				cl.remote.Parity = mode.Parity
			}
		}
		if cl.remote.StopBits != mode.StopBits {
			if w.stopBits(cl.c) == nil {
				cl.remote.StopBits = mode.StopBits
			}
		}
	}
	return
}

func (w *SerialWorker) Mode() serial.Mode {
	return w.mode
}

func (w *SerialWorker) SerialClose() error {
	if !w.connected {
		return nil
	}
	w.connected = false
	return SerialClose(w.serialConn)
}

// Когда ждать connectSerial не хорошо
func (w *SerialWorker) SetSerial(con serial.Port) {
	w.connected = false
	if w.quitting {
		return
	}
	w.serialConn = con
	w.connected = true
}

func (w *SerialWorker) connectSerial() {
	w.connected = false
	if w.quitting {
		return
	}
	// log.Println("connectSerial...")
	if len(w.args) > 0 || !SerialPath(w.path) {
		// -Hcmd -H:2323
		if w.like != nil {
			w.connected = true
			return
		}
		var err error
		w.serialConn, w.like, err = openLike(w)
		w.connected = err == nil
		return
	}
	// Poll on Serial to open (Testing)
	con, err := serial.Open(w.path, &w.mode)
	for err != nil {
		select {
		case <-w.context.Done():
			w.quitting = true
			// log.Println("...connectSerial")
			return
		case <-time.After(time.Second):
			con, err = serial.Open(w.path, &w.mode)
		}
	}

	w.serialConn = con
	w.connected = true
	// log.Println("...connectSerial")
}

func (w *SerialWorker) txWorker() {
	if w.quitting {
		return
	}
	// log.Println("txWorker...")
	defer func() {
		w.quitting = true
		// log.Println("...txWorker")
	}()
	for {
		select {
		case <-w.context.Done():
			return
		case job, ok := <-w.txJobQueue:
			if !ok {
				return
			}
			if w.connected {
				_, err := w.serialConn.Write([]byte{job})
				if err != nil {
					// w.connected = false

					porterr, ok := err.(serial.PortError)
					if ok {
						log.Printf("ERR: Writing failed %s\r\n", porterr.EncodedErrorString())
						w.lastErr = porterr.EncodedErrorString()
					}
					// log.Printf("txWorker w.SerialClose")
					w.SerialClose()
					// w.Stop()
				}
			} else if job == '\n' {
				err := fmt.Sprintf("Error: %s\n", w.lastErr)
				for _, c := range []byte(err) {
					w.mux.Lock()
					for i := range w.rxJobQueue {
						w.rxJobQueue[i] <- c
					}
					w.mux.Unlock()
				}
			}
		}
	}
}

func (w *SerialWorker) rxWorker() {
	if w.quitting {
		return
	}
	// log.Println("rxWorker...")

	b := make([]byte, k1) // B16
	defer func() {
		w.quitting = true
		// log.Println("...rxWorker")
	}()

	// Transmit to telnet
	for !w.quitting {
		select {
		case <-w.context.Done():
			return
		case <-time.After(time.Millisecond):
			n, err := w.serialConn.Read(b)

			if n > 0 {
				w.mux.Lock()
				for j := 0; j < n; j++ {
					for i := range w.rxJobQueue {
						w.rxJobQueue[i] <- b[j]
					}
				}
				w.mux.Unlock()
			}

			if err != nil {
				if !w.connected {
					return
				}
				if !w.quitting {
					if err == syscall.EINTR {
						continue
					}
					if err == io.EOF || strings.Contains(err.Error(), "/dev/ptmx:") {
						log.Printf("%v\r\n", err)
					} else {
						log.Printf("error reading from serial: %v\r\n", err)
					}
					w.Cancel()
					porterr, ok := err.(serial.PortError)
					if ok {
						log.Printf("ERR: Reading failed %s\r\n", porterr.EncodedErrorString())
						w.lastErr = porterr.EncodedErrorString()
					}
				}
				// log.Printf("rxWorker w.SerialClose")
				w.SerialClose()
				// w.Stop()
				return
			}
		}
	}
}

// Worker is the worker operating the serial port
func (w *SerialWorker) Worker() {
	if w.quitting {
		return
	}
	// log.Println("Worker...")
	// Receive from telnet
	go w.txWorker()
	for !w.quitting {
		w.connectSerial()

		// Transmit to telnet
		go w.rxWorker()

		var err error
		if w.like == nil || w.like.command {
			_, err = os.Stat(w.path)
		}

	loop:
		// Windows after open serial port block access to it
		// do not w.serialConn.Close if err == nil || runtime.GOOS == "windows" && strings.HasSuffix(err.Error(), "Access is denied.")
		for w.connected && (err == nil || runtime.GOOS == "windows" && strings.HasSuffix(err.Error(), "Access is denied.")) {
			select {
			case <-w.context.Done():
				w.quitting = true
				break loop
			case <-time.After(time.Second):
				if w.like == nil || w.like.command {
					_, err = os.Stat(w.path)
				}
			}
		}
		// log.Printf("Worker w.SerialClose")
		w.SerialClose()
	}
	// log.Println("...Worker")
	// w.Stop()
}

// Serve is invoked by an external entity to provide a Reader and Writer interface
func (w *SerialWorker) serve(context context.Context, wr io.Writer, rr io.Reader) {
	// log.Println("serve...")
	var wg sync.WaitGroup
	wg.Add(2)

	rx := make(chan byte, k32)

	// Add RX fifo
	w.mux.Lock()
	w.rxJobQueue = append(w.rxJobQueue, rx)
	w.mux.Unlock()

	go func() {
		// log.Println("serve Write...")
		var lastchar byte
		defer func() {
			// w.quitting = true
			rr.(*telnet.Connection).Close()
			// log.Println("...serve Write")
			wg.Done()
		}()

		for !w.quitting {
			select {
			case <-context.Done():
				return
			case b, ok := <-rx:
				if !ok {
					return
				}
				// \r\n -> \r\n
				// x\n -> x\r\n
				// com->telnet
				if b == '\n' && lastchar != '\r' {

					_, err := wr.Write([]byte{'\r'})
					if err != nil {
						return
					}
				}
				_, err := wr.Write([]byte{b})
				if err != nil {
					return
				}
				lastchar = b
			}
		}
	}()
	go func() {
		// log.Println("serve Read...")
		p := make([]byte, k1)
		defer func() {
			// w.quitting = true
			wr.(*telnet.Connection).Close()
			// log.Println("...serve Read")
			wg.Done()
		}()

		for !w.quitting {
			select {
			case <-context.Done():
				return
			case <-time.After(time.Millisecond):
				n, err := rr.Read(p)
				if err != nil && strings.Contains(strings.ToLower(err.Error()), "i/o timeout") {
					time.Sleep(time.Microsecond)
					continue
				} else if err != nil {
					return
				}
				buf := bytes.ReplaceAll(p[:n], []byte("\r\n"), []byte("\r"))
				for _, b := range buf {
					w.txJobQueue <- b
				}

			}
		}
	}()

	wg.Wait()

	// Remove RX fifo
	w.mux.Lock()
	var new []chan byte

	for i := range w.rxJobQueue {
		if w.rxJobQueue[i] != rx {
			new = append(new, w.rxJobQueue[i])
		}
	}
	w.rxJobQueue = new
	w.mux.Unlock()
	// log.Println("...serve")
}

// ServeTELNET is the worker operating the telnet port - used by reiver/go-telnet
func (w *SerialWorker) HandleTelnet(conn *telnet.Connection) {
	// log.Println("HandleTelnet...")
	if w.quitting {
		return
	}
	w.serve(w.context, conn, conn)
	conn.Close()
	// log.Println("...HandleTelnet")
}

// Close removes the channel from the internal list
func (w *SerialWorker) Close(rx chan byte) {
	// Remove RX fifo
	w.mux.Lock()
	var new []chan byte

	for i := range w.rxJobQueue {
		if w.rxJobQueue[i] != rx {
			new = append(new, w.rxJobQueue[i])
		}
	}
	w.rxJobQueue = new
	w.mux.Unlock()
}

// Open adds a channel to the internal list
func (w *SerialWorker) Open() (rx chan byte) {
	rx = make(chan byte, k32)

	// Add RX fifo
	w.mux.Lock()
	w.rxJobQueue = append(w.rxJobQueue, rx)
	w.mux.Unlock()

	return
}

// Name returns the instance name
func (w *SerialWorker) Name() (name string) {
	// name = "go-ser2net"
	return w.path
}

// SerialIOWorker used as GoTTY factory
type SerialIOWorker struct {
	w          *SerialWorker
	rx         chan byte
	lastRxchar byte
	// lastTxchar byte
}

// Read implements gotty slave interface
func (g *SerialIOWorker) Read(buffer []byte) (n int, err error) {
	var b byte

	b = <-g.rx

	for {
		// Заменяем x\n на x\r\n
		// Но \r\n передаём как \r\n
		if b == '\n' && g.lastRxchar != '\r' {
			if n < len(buffer) {
				buffer[n] = '\r'
				n++
			}
		}
		if n < len(buffer) {
			buffer[n] = b
			n++
		}

		g.lastRxchar = b
		if n == len(buffer) {
			break
		}

		// Receive more characters if any
		select {
		case b = <-g.rx:
		default:
			return
		}
	}

	return
}

// Write implements gotty slave interface
func (g *SerialIOWorker) Write(buffer []byte) (n int, err error) {
	for _, p := range buffer {
		if p == 0x7f { // ^H или BackSpace
			p = '\b'
		}
		g.w.txJobQueue <- p
	}

	return len(buffer), nil // если изменить n то будет ошибка
}

// Close implements gotty slave interface
func (g *SerialIOWorker) Close() (err error) {
	g.w.Close(g.rx)
	return
}

// ResizeTerminal implements gotty slave interface
func (g SerialIOWorker) ResizeTerminal(columns int, rows int) (err error) {
	return
}

// WindowTitleVariables implements gotty slave interface
func (g SerialIOWorker) WindowTitleVariables() (titles map[string]interface{}) {
	titles = map[string]interface{}{
		"command": g.w.Name(),
	}
	return
}

// New returns a GoTTY slave
func (w *SerialWorker) New(params map[string][]string, _ map[string][]string) (s server.Slave, err error) {
	rx := w.Open()
	s = &SerialIOWorker{
		w:  w,
		rx: rx,
	}

	return
}

// NewIoReadWriteCloser returns a ReadWriteCloser interface
const TOopen = 200

func (w *SerialWorker) NewIoReadWriteCloser() (s io.ReadWriteCloser, err error) {
	const TOtryOpen = 10
	for i := 0; i < TOopen/TOtryOpen; i++ {
		if w.connected {
			// log.Println(w, "in", i*TOtryOpen, "milliseconds\r")
			rx := w.Open()
			s = &SerialIOWorker{
				w:  w,
				rx: rx,
			}
			w.context, w.cancel = context.WithCancel(w.context)
			return
		}
		time.Sleep(time.Millisecond * TOtryOpen)
	}
	err = fmt.Errorf("not connected to %s in %d milliseconds. Last error:%s", w.path, TOopen, w.lastErr)
	return
}

// StartGoTTY starts a GoTTY server
func (w *SerialWorker) StartGoTTY(address string, port int, basicauth string, quiet bool) (err error) {
	if w.quitting {
		return
	}

	appOptions := &server.Options{}
	if err = utils.ApplyDefaultValues(appOptions); err != nil {
		return
	}
	appOptions.PermitWrite = true
	appOptions.Address = address
	appOptions.EnableReconnect = true
	appOptions.Port = fmt.Sprintf("%d", port)
	appOptions.EnableBasicAuth = len(basicauth) > 0
	appOptions.Credential = basicauth

	appOptions.Quiet = quiet

	// if appOptions.Quiet {
	// 	log.SetOutput(io.Discard)
	// }

	hostname, _ := os.Hostname()

	appOptions.TitleVariables = map[string]interface{}{
		"command":  os.Args[0],
		"argv":     os.Args[1:],
		"hostname": hostname,
	}

	err = appOptions.Validate()
	if err != nil {
		return
	}

	srv, err := server.New(w, appOptions)
	if err != nil {
		return
	}

	w.web = srv
	w.context, w.cancel = context.WithCancel(w.context)
	w.url = fmt.Sprintf("http://%s", net.JoinHostPort(appOptions.Address, appOptions.Port))
	err = srv.Run(w.context)
	// log.Printf("StartGoTTY w.Stop")
	w.Stop()
	w.web = nil

	if err != nil {
		w.lastErr = err.Error()
		time.Sleep(time.Millisecond * 111)
	}

	return
}

// StartTelnet starts a RFC2217 telnet server
func (w *SerialWorker) StartTelnet(bindHostname string, port int) (err error) {
	// log.Println("StartTelnet...")
	if w.quitting {
		return
	}
	w.context, w.cancel = context.WithCancel(w.context)
	Server := w.Server2217
	if len(w.args) > 0 {
		Server = w.Server1073
	}
	w.rfc2217 = telnet.NewServer(fmt.Sprintf("%s:%d", bindHostname, port), w, options.EchoOption, options.SuppressGoAheadOption, options.BinaryTransmissionOption, w.Server727, Server)
	w.url = "telnet://" + w.rfc2217.Address
	err = w.rfc2217.ListenAndServe()
	w.rfc2217 = nil
	// log.Printf("StartTelnet w.Stop")
	w.Stop()

	if err != nil {
		w.lastErr = err.Error()
		time.Sleep(time.Millisecond * 111)
	}
	// log.Println("...StartTelnet")
	return
}

var DefaultMode = serial.Mode{
	BaudRate: 9600,
	DataBits: 8,
	Parity:   serial.NoParity,
	StopBits: serial.OneStopBit,

	// if mode.InitialStatusBits == nil {
	// 	params.Flags |= dcbDTRControlEnable
	// 	params.Flags |= dcbRTSControlEnable
	// } else {
	// 	if mode.InitialStatusBits.DTR {
	// 		params.Flags |= dcbDTRControlEnable
	// 	}
	// 	if mode.InitialStatusBits.RTS {
	// 		params.Flags |= dcbRTSControlEnable
	// 	}
	// }
	InitialStatusBits: &serial.ModemOutputBits{},
}

// NewSerialWorker creates a new SerialWorker and connect to path@baud,8,N,1,N.
// Если path пустой то это для клиента RFC2217.
func NewSerialWorker(context context.Context, path string, baud int) (*SerialWorker, error) {
	var w SerialWorker
	w.context = context
	w.cls = make(map[string]*Client)
	w.lastErr = fmt.Sprintf("%s is not connected", path)
	w.path = path

	args, ok := IsCommand(path)
	if ok {
		// Команда или интерпретатор команд
		w.path = args[0]
		w.args = args
		w.lastErr = fmt.Sprintf("Command %v not started", args)
	} else if _, _, err := net.SplitHostPort(LocalPort(path)); err == nil {
		// Клиент telnet
		w.path = LocalPort(path)
		w.lastErr = fmt.Sprintf("Serial or command over telnet://%s is not connected", w.path)
	} else if SerialPath(path) {
		// Последовательный порт
		w.lastErr = fmt.Sprintf("Serial %s is not connected", w.path)
	}
	if baud < 0 {
		// Для клиента на сервере
		w.mode = serial.Mode{}
		return &w, nil
	}
	w.txJobQueue = make(chan byte, k4)

	// func (port *windowsPort) setModeParams(mode *Mode, params *dcb) {
	// 	if mode.BaudRate == 0 {
	// 		params.BaudRate = 9600 // Default to 9600
	// 	} else {
	// 		params.BaudRate = uint32(mode.BaudRate)
	// 	}
	// }

	w.mode = DefaultMode
	if baud > 0 {
		w.mode.BaudRate = baud
	}

	return &w, nil
}

// baudRate(strconv.Atoi("x"))
func BaudRate(b int, err error) (baud int) {
	if err != nil {
		baud = 9600
		return
	}
	switch b {
	case 0:
		baud = 115200
	case 1:
		baud = 19200
	case 2:
		baud = 2400
	case 3:
		baud = 38400
	case 4:
		baud = 4800
	case 5:
		baud = 57600
	case 9:
		baud = 9600
	default:
		baud = b
	}
	return
}

type likeSerialPort struct {
	closed bool
	// -H:2323
	conn *telnet.Connection
	// -Hcmd
	command bool
	console console.Console
	ws      WinSize
}

func openLike(w *SerialWorker) (port serial.Port, l *likeSerialPort, err error) {
	l = &likeSerialPort{}
	l.command = len(w.args) > 0
	if l.command {
		// -Hcmd -Hbash
		ws, _ := size()
		l.console, err = console.New(int(ws.Width), int(ws.Height))
		// log.Println(l, err)
		if err != nil {
			return nil, nil, err
		}
		err = l.console.Start(w.args)
		if err != nil {
			return nil, nil, err
		}
		w.pid, _ = l.console.Pid()
		return l, l, err
	}
	// -H:2323
	l.conn, err = telnet.Dial(w.path, w.Client727, w.Client1073, w.Client2217)

	if err != nil {
		w.quitting = true
		return nil, nil, err
	}
	return l, l, err
}

func (likeSerialPort) Break(time.Duration) error {
	return nil
}
func (l *likeSerialPort) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	// log.Println("likeSerialPort.Close")

	if l.command {
		if l.console == nil {
			// log.Println("l.console == nil")
			return nil
		}
		err := l.console.Close()
		// log.Println("l.console.Close")
		return err
	}
	if l.conn == nil {
		// log.Println("l.conn == nil")
		return nil
	}
	IAC(l.conn, telnet.DO, telnet.TeloptLOGOUT)
	// time.Sleep(time.Millisecond * 11)
	err := l.conn.Close()
	// log.Println("l.conn.Close")
	return err
}

func (likeSerialPort) Drain() error {
	return nil
}
func (likeSerialPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return nil, nil
}
func (l likeSerialPort) Read(p []byte) (n int, err error) {
	if l.command {
		if l.console == nil {
			return len(p), nil
		}
		return l.console.Read(p)
	}
	if l.conn == nil {
		return len(p), nil
	}
	return l.conn.Read(p)
}
func (likeSerialPort) ResetInputBuffer() error {
	return nil
}
func (likeSerialPort) ResetOutputBuffer() error {
	return nil
}
func (likeSerialPort) SetDTR(dtr bool) error {
	return nil
}
func (likeSerialPort) SetRTS(rts bool) error {
	return nil
}

func (l likeSerialPort) SetMode(mode *serial.Mode) error {
	return nil
}
func (likeSerialPort) SetReadTimeout(t time.Duration) error {
	return nil
}
func (l likeSerialPort) Write(p []byte) (n int, err error) {
	if l.command {
		if l.console == nil {
			return len(p), nil
		}
		return l.console.Write(p)
	}
	if l.conn == nil {
		return len(p), nil
	}
	return l.conn.Write(p)
}

type WinSize struct {
	// Height of the console
	Height uint16
	// Width of the console
	Width uint16
}

func isFileExist(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

// Похож ли path на команду
func IsCommand(path string) (args []string, ok bool) {
	args, err := splitCommandLine(path)
	if err == nil {
		args[0], err = exec.LookPath(args[0])
		ok = err == nil && isFileExist(args[0]) && !SerialPath(path)
	}
	return
}

// Похож ли path на последовательную консоль
func SerialPath(path string) bool {
	if _, _, err := net.SplitHostPort(path); err == nil || strings.Contains(path, " ") {
		// if strings.Contains(path, " ") || strings.Contains(path, ":") {
		return false
	}
	return LastDigit(path)
}

func LastDigit(path string) bool {
	if path == "" {
		return false
	}
	suff := path[len(path)-1:]
	return suff >= "0" && suff <= "9"
}

// https://github.com/northbright/iocopy/blob/master/iocopy.go
// readFunc is used to implement [io.Reader] interface and capture the [context.Context] parameter.
type readFunc func(p []byte) (n int, err error)

// https://github.com/northbright/iocopy/blob/master/iocopy.go
// Read implements [io.Reader] interface.
func (rf readFunc) Read(p []byte) (n int, err error) {
	return rf(p)
}

// https://github.com/northbright/iocopy/blob/master/iocopy.go
// Copy wraps [io.Copy] and accepts [context.Context] parameter.
// Копирует пока в контексте.
func Copy(ctx context.Context, dst io.Writer, src io.Reader) (written int64, err error) {
	return io.Copy(
		dst,
		readFunc(func(p []byte) (n int, err error) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
				return src.Read(p)
			}
		}),
	)
}

// Copy wraps [io.Copy] and accepts [context.Context]  and delay parameters.
// Копирует пока в контексте с отложенным на delay src.Read.
func CopyAfter(ctx context.Context, dst io.Writer, src io.Reader, delay time.Duration) (written int64, err error) {
	return io.Copy(
		dst,
		readFunc(func(p []byte) (n int, err error) {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return 0, ctx.Err()
			case <-t.C:
				return src.Read(p)
			}
		}),
	)
}

// Копирует и после завершения вызывает cancel заданный в w.NewCancel(cancel)
func (w *SerialWorker) CopyCancel(dst io.Writer, src io.Reader) (written int64, err error) {
	// written, err = io.Copy(dst, src)
	written, err = w.Copy(dst, src)
	// log.Printf("CopyCancel done\r\n")
	w.Cancel()
	return
}

type ReadWriteCloser struct {
	io.Reader
	io.WriteCloser
}

func (w *SerialWorker) CancelCopy(dst io.Writer, src io.ReadCloser) (written int64, err error) {
	s := src
	_, like := w.serialConn.(*likeSerialPort)
	_, local := src.(ReadWriteCloser)
	if like {
		if local {
			w.in, err = NewStdin()
			if err == nil {
				s = w.in
				// log.Printf("LikeSerial & Local %+v\r\n", w.in)
				defer func() {
					w.in.Close()
					w.in = nil
				}()
			}
		} else {
			w.in = &Stdin{cancel: src.Close}
			// log.Printf("LikeSerial & !Local %+v\r\n", w.in)
		}
	}
	// written, err = io.Copy(dst, s)
	written, err = w.Copy(dst, s)
	// log.Printf("CancelCopy done\r\n")
	if err != nil && err.Error() == "The handle is invalid." {
		err = fmt.Errorf("read canceled")
	}
	return
}

// Устанавливает функцию cancel для прекращения встречного копирования.
func (w *SerialWorker) NewCancel(cancel func() error) {
	w.in = &Stdin{cancel: cancel}
}

// Копирует пока в контексте SerialWorker.
// Потом выходит из контекста чтоб прервать встречное копирование.
func (w *SerialWorker) Copy(dst io.Writer, src io.Reader) (written int64, err error) {
	written, err = Copy(w.context, dst, src)
	w.cancel()
	return
}

// Копирует пока в контексте SerialWorker с отложенный на delay src.Read.
// Потом выходит из контекста чтоб прервать встречное копирование.
func (w *SerialWorker) CopyAfter(dst io.Writer, src io.Reader, delay time.Duration) (written int64, err error) {
	written, err = CopyAfter(w.context, dst, src, delay)
	w.cancel()
	return
}

// Используется после установки через NewCancel для прерывания копирования.
func (w *SerialWorker) Cancel() (ok bool) {
	w.Stop()
	if w.in != nil {
		ok = w.in.Cancel()
	}
	return
}

// Заменяет в addr локальные алиасы "", *, +, _ для dial.
// "123"->"127.0.0.1:123".
// ":123"->"127.0.0.1:123".
// "*:123"->"firstUpInt:123".
// "+:123"->"firstUpInt:123".
// "_:123"->"lastUpInt:123".
func LocalPort(addr string) string {
	addr = strings.TrimPrefix(addr, ":")
	if _, err := strconv.ParseUint(addr, 10, 16); err == nil {
		return lh + ":" + addr
	}
	if strings.HasPrefix(addr, "_:") ||
		strings.HasPrefix(addr, "*:") || strings.HasPrefix(addr, "+:") || strings.HasPrefix(addr, "0.0.0.0:") {
		ips := Ints()
		if strings.HasPrefix(addr, "_:") {
			addr = strings.TrimPrefix(addr, "_:")
			return ips[len(ips)-1] + ":" + addr
		}
		addr = strings.TrimPrefix(addr, "*:")
		addr = strings.TrimPrefix(addr, "+:")
		addr = strings.TrimPrefix(addr, "0.0.0.0:")
		return ips[0] + ":" + addr
	}
	return addr
}

// Пишет в ips адреса поднятых интерфейсов без лупбэка.
// Если таких нет тогда лупбэк.
func Ints() (ips []string) {
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil || iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagRunning == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			for _, addr := range addrs {
				if strings.Contains(addr.String(), ":") {
					continue
				}
				ips = append(ips, strings.Split(addr.String(), "/")[0])
			}
		}
	}
	if len(ips) == 0 {
		ips = append(ips, lh)
	}
	return
}
