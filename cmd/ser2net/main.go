package main

import (
	"bufio"
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abakum/go-ser2net/pkg/ser2net"
	"github.com/mattn/go-isatty"
	"github.com/xlab/closer"

	"github.com/containerd/console"
)

func main() {
	log.SetFlags(log.Llongfile | log.Lmicroseconds)
	log.SetPrefix("\r")
	devPath := `COM3`
	// devPath := `cmd`
	// devPath := `:2322`
	configPath := ""
	bindHostname := ""
	telnet := false
	gotty := false
	stdin := true
	baud := 115200
	once := false

	flag.StringVar(&bindHostname, "bind", bindHostname, "Hostname or IP to bind telnet to")
	flag.StringVar(&devPath, "dev", devPath, "TTY to open")
	flag.StringVar(&configPath, "config", configPath, "TTY to open")
	flag.IntVar(&port, "port", port, "Telnet port")
	flag.IntVar(&baud, "baud", baud, "Baud rate")
	useTelnet := flag.Bool("telnet", telnet, "Use telnet")
	useGotty := flag.Bool("gotty", gotty, "Use GoTTY")
	useStdin := flag.Bool("stdin", stdin, "Use stdin/stdout")

	flag.Parse()
	if devPath == "" && configPath == "" {
		flag.Usage()
		panic("Error: Device path not set and config not given")
	}
	switch bindHostname {
	case "*":
		bindHostname = "0.0.0.0"
	case "":
		bindHostname = "127.0.0.1"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer closer.Close()
	closer.Bind(func() {
		if ctx.Err() == nil {
			cancel()
		}
	})

	if configPath != "" {
		var wg sync.WaitGroup

		file, err := os.Open(configPath)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {

			if len(scanner.Text()) == 0 {
				continue
			}
			if strings.HasPrefix(scanner.Text(), "BANNER") {
				continue
			}
			if strings.Contains(scanner.Text(), ":telnet") {
				conf := strings.Split(scanner.Text(), ":")

				if len(conf) < 4 {
					continue
				}
				if conf[1] != "telnet" {
					continue
				}
				port, _ := strconv.Atoi(conf[0])
				devPath = conf[3]
				// baud := 115200

				var opts []string
				if len(conf) > 4 {
					opts = strings.Split(conf[4], " ")
				}
				if len(opts) > 0 {
					baud, _ = strconv.Atoi(opts[0])
				}
				log.Printf("telnet on port %d baud %d, device %s\n", port, baud, devPath)
				w, _ := ser2net.NewSerialWorker(ctx, devPath, baud)
				go w.Worker()

				go func() {
					defer wg.Done()

					err = w.StartTelnet(bindHostname, port)
					if nil != err {
						panic(err)
					}
				}()
				wg.Add(1)
			} else if strings.Contains(scanner.Text(), ":gotty") {
				conf := strings.Split(scanner.Text(), ":")

				if len(conf) < 4 {
					continue
				}
				if conf[1] != "gotty" {
					continue
				}
				port, _ := strconv.Atoi(conf[0])
				devPath = conf[3]

				var opts []string
				if len(conf) > 4 {
					opts = strings.Split(conf[4], " ")
				}
				if len(opts) > 0 {
					baud, _ = strconv.Atoi(opts[0])
				}
				log.Printf("gotty on port %d baud %d, device %s\n", port, baud, devPath)
				w, _ := ser2net.NewSerialWorker(ctx, devPath, baud)
				go w.Worker()

				go func() {
					defer wg.Done()
					err := w.StartGoTTY(bindHostname, port, "", true)
					if nil != err {
						panic(err)
					}
				}()
				wg.Add(1)
			}
		}

		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
		wg.Wait()

	} else {
		w, _ := ser2net.NewSerialWorker(ctx, devPath, baud)
		// go func() {
		// 	for i := 0; i < 10; i++ {
		// 		log.Println(i, w)
		// 		time.Sleep(time.Second)
		// 	}
		// 	cancel()
		// 	for i := 0; i < 10; i++ {
		// 		log.Println(i, w)
		// 		time.Sleep(time.Second)
		// 	}
		// }()
		go w.Worker()

		go func() {
			o := ""
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
					n := w.String()
					if o != n {
						log.Print(n, "\r\n")
						o = n
					}
				}
			}
		}()

		if useTelnet != nil && *useTelnet {
			err := w.StartTelnet(bindHostname, port)
			// time.Sleep(time.Second * 5)
			if nil != err {
				panic(err)
			}
		} else if useGotty != nil && *useGotty {
			err := w.StartGoTTY(bindHostname, port, "", false)
			if nil != err {
				panic(err)
			}
		} else if useStdin != nil && *useStdin {

			// Get a ReadWriteCloser interface
			i, err := w.NewIoReadWriteCloser()
			if nil != err {
				panic(err)
			}
			defer i.Close()
			setRaw(&once)
			// Copy serial out to stdout
			go func() {
				w.Copy(os.Stdout, i)
			}()

			w.CopyAfter(i, os.Stdin, time.Millisecond*77)

		} else {
			panic("Must specify one of [telnet, gotty]")
		}
	}

}

func setRaw(already *bool) {
	if *already {
		return
	}
	*already = true

	var (
		err      error
		current  console.Console
		settings string
	)

	current, err = console.ConsoleFromFile(os.Stdin)
	if err == nil {
		err = current.SetRaw()
		if err == nil {
			closer.Bind(func() { current.Reset() })
			return
		}
	}

	if isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		settings, err = sttySettings()
		if err == nil {
			err = sttyMakeRaw()
			if err == nil {
				closer.Bind(func() { sttyReset(settings) })
				return
			}
		}
	}

}

func sttyMakeRaw() error {
	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func sttySettings() (string, error) {
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sttyReset(settings string) {
	cmd := exec.Command("stty", settings)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}
