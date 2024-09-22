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
	B16 = 16
	K1  = 1024
	K4  = 4 * 1024
	K32 = 32 * 1024
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
	rfc2217  *telnet.Server
	web      *server.Server
	url      string
	quitting bool
	shell    *likeSerialPort
	args     []string
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
	Name string
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
	return w.Name
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
	name := ""
	if m.Name != "" {
		name += m.Name + "@"
	}
	if m.DataBits == 0 {
		return fmt.Sprintf("%s%d",
			name, m.BaudRate)
	}
	return fmt.Sprintf("%s%d,%d,%s,%s",
		name, m.BaudRate, m.DataBits, p, s)
}

func (w *SerialWorker) String() string {
	connected := "connected"
	if !w.connected {
		connected = "not " + connected
	}
	if w.url != "" {
		connected += " to " + w.url
	}
	return fmt.Sprintf("%s %s",
		Mode{w.mode, w.path}, connected)
}

// Останавливает сервер w.rfc2217.
// Останавливает сервер w.web через w.cancel().
// Останавливает последовательный порт w.cancel().
func (w *SerialWorker) Stop() {
	w.url = ""
	if w.cancel != nil {
		w.cancel()
	}
	if w.rfc2217 != nil {
		w.rfc2217.Stop()
	}
}

func (w *SerialWorker) SetMode(mode *serial.Mode) error {
	w.mode = *mode
	return w.serialConn.SetMode(mode)
}

func (w *SerialWorker) Mode() serial.Mode {
	return w.mode
}

func (w *SerialWorker) SerialClose() error {
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
	if len(w.args) > 0 {
		// log.Println(w.path, w.args)
		if w.shell != nil {
			w.connected = true
			return
		}
		var err error
		w.serialConn, w.shell, err = open(w)
		// log.Println(w.args, err)
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
		default:
			time.Sleep(time.Second)
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
					w.connected = false

					porterr, ok := err.(serial.PortError)
					if ok {
						log.Printf("ERR: Writing failed %s\r\n", porterr.EncodedErrorString())
						w.lastErr = porterr.EncodedErrorString()
					}
					w.SerialClose()
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

	b := make([]byte, B16)
	defer func() {
		w.quitting = true
		// log.Println("...rxWorker")
	}()

	// Transmit to telnet
	for !w.quitting {
		select {
		case <-w.context.Done():
			return
		default:
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
				if !w.quitting {
					if err == syscall.EINTR {
						continue
					}
					if err == io.EOF {
						log.Printf("%v Press <Enter><Enter>\r\n", err)
					} else {
						log.Printf("error reading from serial: %v\r\n", err)
					}
					w.connected = false

					porterr, ok := err.(serial.PortError)
					if ok {
						log.Printf("ERR: Reading failed %s\r\n", porterr.EncodedErrorString())
						w.lastErr = porterr.EncodedErrorString()
					}
				}
				w.SerialClose()
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

		_, err := os.Stat(w.path)

	loop:
		// Windows after open serial port block access to it
		// do not w.serialConn.Close if err == nil || runtime.GOOS == "windows" && strings.HasSuffix(err.Error(), "Access is denied.")
		for w.connected && (err == nil || runtime.GOOS == "windows" && strings.HasSuffix(err.Error(), "Access is denied.")) {
			select {
			case <-w.context.Done():
				w.quitting = true
				break loop
			default:
				time.Sleep(time.Second)
				_, err = os.Stat(w.path)
			}
		}
		w.SerialClose()
	}
	// log.Println("...Worker")
	w.Stop()
}

// Serve is invoked by an external entity to provide a Reader and Writer interface
func (w *SerialWorker) serve(context context.Context, wr io.Writer, rr io.Reader) {
	// log.Println("serve...")
	var wg sync.WaitGroup
	wg.Add(2)

	rx := make(chan byte, K32)

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
		p := make([]byte, K1)
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
			default:
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
	rx = make(chan byte, K32)

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
	s = &SerialIOWorker{w: w,
		rx: rx,
	}

	return
}

// NewIoReadWriteCloser returns a ReadWriteCloser interface
func (w *SerialWorker) NewIoReadWriteCloser() (s io.ReadWriteCloser, err error) {
	for i := 0; i < 20; i++ {
		if w.connected {
			log.Println(w.path, "connected in", i*10, "milliseconds\r")
			rx := w.Open()
			s = &SerialIOWorker{w: w,
				rx: rx,
			}

			return
		}
		time.Sleep(time.Millisecond * 10)
	}
	err = fmt.Errorf("not connected to %s in 200 milliseconds. Last error:%s", w.path, w.lastErr)
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

	if appOptions.Quiet {
		log.SetOutput(io.Discard)
	}

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
	ctx, cancel := context.WithCancel(w.context)
	w.context = ctx
	w.cancel = cancel
	w.url = fmt.Sprintf("http://%s", net.JoinHostPort(appOptions.Address, appOptions.Port))
	err = srv.Run(w.context)
	w.Stop()
	w.web = nil

	if err != nil {
		w.lastErr = err.Error()
		time.Sleep(time.Millisecond * 111)
	}

	return
}

// StartTelnet starts a telnet server
func (w *SerialWorker) StartTelnet(bindHostname string, port int) (err error) {
	// log.Println("StartTelnet...")
	if w.quitting {
		return
	}
	ctx, cancel := context.WithCancel(w.context)
	w.context = ctx
	w.cancel = cancel
	w.rfc2217 = telnet.NewServer(fmt.Sprintf("%s:%d", bindHostname, port), w, options.EchoOption, options.SuppressGoAheadOption, options.BinaryTransmissionOption)
	w.url = "telnet://" + w.rfc2217.Address
	err = w.rfc2217.ListenAndServe()
	w.rfc2217 = nil
	w.Stop()

	if err != nil {
		w.lastErr = err.Error()
		time.Sleep(time.Millisecond * 111)
	}
	// log.Println("...StartTelnet")
	return
}

// NewSerialWorker creates a new SerialWorker and connect to path@115200,8,N,1
func NewSerialWorker(context context.Context, path string, baud int) (*SerialWorker, error) {
	var w SerialWorker
	w.txJobQueue = make(chan byte, K4)
	if baud <= 0 {
		baud = 115200
	}
	w.mode.BaudRate = baud
	w.mode.DataBits = 8
	w.mode.Parity = serial.NoParity
	w.mode.StopBits = serial.OneStopBit
	w.path = path
	w.connected = false
	w.lastErr = "Serial is not connected"
	w.context = context
	w.quitting = false

	// Команда или шелл
	args, ok := IsCommand(path)
	if ok {
		w.path = args[0]
		w.args = args
		w.lastErr = fmt.Sprintf("Command %v not started", args)
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
	console console.Console
	closed  bool
}

func open(w *SerialWorker) (port serial.Port, sh *likeSerialPort, err error) {
	sh = &likeSerialPort{}
	ws, err := size()
	if err != nil {
		ws.Width = 80
		ws.Height = 25
	}
	sh.console, err = console.New(int(ws.Width), int(ws.Height))
	// log.Println(sh, err)
	if err != nil {
		return nil, nil, err
	}
	err = sh.console.Start(w.args)
	if err != nil {
		return nil, nil, err
	}
	w.mode.BaudRate, _ = sh.console.Pid()
	w.mode.DataBits = 0
	return sh, sh, err
}

func (likeSerialPort) Break(time.Duration) error {
	return nil
}
func (l *likeSerialPort) Close() error {
	if l.console == nil {
		return nil
	}
	if l.closed {
		return nil
	}
	l.closed = true
	return l.console.Close()
}
func (likeSerialPort) Drain() error {
	return nil
}
func (likeSerialPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return nil, nil
}
func (l likeSerialPort) Read(p []byte) (n int, err error) {
	if l.console == nil {
		return len(p), nil
	}
	return l.console.Read(p)
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
func (likeSerialPort) SetMode(mode *serial.Mode) error {
	return nil
}
func (likeSerialPort) SetReadTimeout(t time.Duration) error {
	return nil
}
func (l likeSerialPort) Write(p []byte) (n int, err error) {
	if l.console == nil {
		return len(p), nil
	}
	return l.console.Write(p)
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

func IsCommand(path string) (args []string, ok bool) {
	args, err := splitCommandLine(path)
	if err == nil {
		arg0 := args[0]
		args[0], err = exec.LookPath(args[0])
		ok = err == nil && isFileExist(args[0]) && !serialPath(arg0)
	}
	return
}

func serialPath(path string) bool {
	if len(path) == 0 {
		return false
	}
	suff := path[len(path)-1:]
	return suff >= "0" && suff <= "9"
}
