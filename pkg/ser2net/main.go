package ser2net

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PatrickRudolph/telnet"
	"github.com/PatrickRudolph/telnet/options"
	"github.com/sorenisanerd/gotty/server"
	"github.com/sorenisanerd/gotty/utils"
	"go.bug.st/serial"
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
	rfc2217  *telnet.Server
	quitting bool
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
	return fmt.Sprintf("%d,%d,%s,%s",
		m.BaudRate, m.DataBits, p, s)
}

func (w *SerialWorker) String() string {
	connected := "connected"
	if !w.connected {
		connected = "not " + connected
	}
	if w.rfc2217 != nil {
		connected += " to " + w.rfc2217.Address
	}
	return fmt.Sprintf("%s@%s %s",
		w.path, Mode{w.mode}, connected)
}

func (w *SerialWorker) Stop() {
	w.quitting = true
	if w.rfc2217 != nil {
		w.rfc2217.Stop()
		w.rfc2217 = nil
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

func (w *SerialWorker) connectSerial() {
	w.connected = false
	if w.quitting {
		return
	}
	// fmt.Println("connectSerial...")

	// Poll on Serial to open (Testing)
	con, err := serial.Open(w.path, &w.mode)
	for err != nil {
		select {
		case <-w.context.Done():
			w.quitting = true
			// fmt.Println("...connectSerial")
			return
		default:
			time.Sleep(time.Second)
			con, err = serial.Open(w.path, &w.mode)
		}
	}

	w.serialConn = con
	w.connected = true
	// fmt.Println("...connectSerial")
}

func (w *SerialWorker) txWorker() {
	if w.quitting {
		return
	}
	// fmt.Println("txWorker...")
	defer func() {
		w.quitting = true
		// fmt.Println("...txWorker")
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
						fmt.Printf("ERR: Writing failed %s\n", porterr.EncodedErrorString())
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
	// fmt.Println("rxWorker...")

	b := make([]byte, 16)
	defer func() {
		w.quitting = true
		// fmt.Println("...rxWorker")
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

					fmt.Printf("error reading from serial: %v\n", err)
					w.connected = false

					porterr, ok := err.(serial.PortError)
					if ok {
						fmt.Printf("ERR: Reading failed %s\n", porterr.EncodedErrorString())
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
	// fmt.Println("Worker...")
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
	// fmt.Println("...Worker")
	w.Stop()
}

// Serve is invoked by an external entity to provide a Reader and Writer interface
func (w *SerialWorker) serve(context context.Context, wr io.Writer, rr io.Reader) {
	// fmt.Println("serve...")
	var wg sync.WaitGroup
	wg.Add(2)

	rx := make(chan byte, 32*1024)

	// Add RX fifo
	w.mux.Lock()
	w.rxJobQueue = append(w.rxJobQueue, rx)
	w.mux.Unlock()

	go func() {
		// fmt.Println("serve Write...")
		var lastchar byte
		defer func() {
			// w.quitting = true
			rr.(*telnet.Connection).Close()
			// fmt.Println("...serve Write")
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
		// fmt.Println("serve Read...")
		p := make([]byte, 16)
		defer func() {
			// w.quitting = true
			wr.(*telnet.Connection).Close()
			// fmt.Println("...serve Read")
			wg.Done()
		}()

		for !w.quitting {
			select {
			case <-context.Done():
				return
			default:
				n, err := rr.Read(p)
				for j := 0; j < n; j++ {
					if p[j] == 0 {
						continue
					}
					// In binary mode there's no special CRLF handling
					// Always transmit everything received
					w.txJobQueue <- p[j]
				}
				if err != nil && strings.Contains(strings.ToLower(err.Error()), "i/o timeout") {
					time.Sleep(time.Microsecond)
					continue
				} else if err != nil {
					return
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
	// fmt.Println("...serve")
}

// ServeTELNET is the worker operating the telnet port - used by reiver/go-telnet
func (w *SerialWorker) HandleTelnet(conn *telnet.Connection) {
	// fmt.Println("HandleTelnet...")
	if w.quitting {
		return
	}
	w.serve(w.context, conn, conn)
	conn.Close()
	// fmt.Println("...HandleTelnet")
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
	rx = make(chan byte, 32*1024)

	// Add RX fifo
	w.mux.Lock()
	w.rxJobQueue = append(w.rxJobQueue, rx)
	w.mux.Unlock()

	return
}

// Name returns the instance name
func (w *SerialWorker) Name() (name string) {
	name = "go-ser2net"
	return
}

// SerialIOWorker used as GoTTY factory
type SerialIOWorker struct {
	w          *SerialWorker
	rx         chan byte
	lastRxchar byte
	lastTxchar byte
}

// Read implements gotty slave interface
func (g *SerialIOWorker) Read(buffer []byte) (n int, err error) {
	var b byte

	b = <-g.rx

	for {
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

		if g.lastTxchar == '\r' && p != '\n' {
			g.w.txJobQueue <- g.lastTxchar
			g.w.txJobQueue <- p
		}
		g.lastTxchar = p
		if p == '\n' {
			g.w.txJobQueue <- '\r'
			n++
			continue
		} else if p == 0x7f {
			g.w.txJobQueue <- '\b'
			n++
			continue
		}
		g.w.txJobQueue <- p
		n++
	}

	return
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
		"command": "go-ser2net",
	}
	return
}

// New returns a GoTTY slave
func (w *SerialWorker) New(params map[string][]string) (s server.Slave, err error) {
	rx := w.Open()
	s = &SerialIOWorker{w: w,
		rx: rx,
	}

	return
}

// NewIoReadWriteCloser returns a ReadWriteCloser interface
func (w *SerialWorker) NewIoReadWriteCloser() (s io.ReadWriteCloser, err error) {
	time.Sleep(time.Millisecond * 3)
	if !w.connected {
		err = fmt.Errorf("not connected to %s. Last error:%s", w.path, w.lastErr)
		return
	}
	rx := w.Open()
	s = &SerialIOWorker{w: w,
		rx: rx,
	}

	return
}

// StartGoTTY starts a GoTTY server
func (w *SerialWorker) StartGoTTY(address string, port int, basicauth string) (err error) {
	htermOptions := &server.HtermPrefernces{}
	appOptions := &server.Options{
		Preferences: htermOptions,
	}
	if err = utils.ApplyDefaultValues(appOptions); err != nil {
		return
	}
	appOptions.PermitWrite = true
	appOptions.Address = address
	appOptions.EnableReconnect = true
	appOptions.Port = fmt.Sprintf("%d", port)
	appOptions.EnableBasicAuth = len(basicauth) > 0
	appOptions.Credential = basicauth
	appOptions.Preferences.BackspaceSendsBackspace = true
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

	err = srv.Run(w.context)
	if err != nil {
		time.Sleep(time.Second)
	}

	return
}

// StartTelnet starts a telnet server
func (w *SerialWorker) StartTelnet(bindHostname string, port int) (err error) {
	// fmt.Println("StartTelnet...")
	if w.quitting {
		return
	}
	ctx, cancel := context.WithCancel(w.context)
	w.context = ctx
	w.rfc2217 = telnet.NewServer(fmt.Sprintf("%s:%d", bindHostname, port), w, options.EchoOption, options.SuppressGoAheadOption, options.BinaryTransmissionOption)
	err = w.rfc2217.ListenAndServe()
	cancel()
	if err != nil {
		w.lastErr = err.Error()
		w.rfc2217 = nil
		time.Sleep(time.Second)
	}
	// fmt.Println("...StartTelnet")
	return
}

// NewSerialWorker creates a new SerialWorker and connect to path with 115200N8
func NewSerialWorker(context context.Context, path string, baud int) (*SerialWorker, error) {
	var w SerialWorker
	w.txJobQueue = make(chan byte, 4096)
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
