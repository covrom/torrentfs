package main

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/anacrolix/envpprof"
	"github.com/anacrolix/tagflag"
	"github.com/covrom/torrentfs/dirwatch"
	"github.com/hekmon/transmissionrpc"
)

const AppVersion = "torrentnotify 1.0"

var (
	args = struct {
		WatchDirs    string       `help:"torrent files locations separated by semicolon"`
		ListenStat   *net.TCPAddr `help:"log is listen on this address:port"`
		RpcURL       string       `help:"transmission rpc server URL: http(s)://usr:pwd@address:port"`
		AliveMinutes int
		Version      bool
	}{
		ListenStat:   &net.TCPAddr{Port: 8800},
		RpcURL:       "http://127.0.0.1:9091",
		AliveMinutes: 240,
		Version:      false,
	}
)

func onShutdown(f func()) {
	once := &sync.Once{}
	sigc := make(chan os.Signal, 3)
	signal.Notify(sigc, os.Interrupt, os.Kill, syscall.SIGTERM)
	go func() {
		<-sigc
		once.Do(f)
	}()
}

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {

	tagflag.Parse(&args)
	if args.Version {
		os.Stdout.WriteString(AppVersion)
		os.Stdout.WriteString("\n")
		return 0
	}
	if args.WatchDirs == "" {
		os.Stderr.WriteString("you no specify watchdirs?\n")
		return 2
	}

	logger, err := os.OpenFile("torrentnotify.log", os.O_APPEND|os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		os.Stderr.WriteString("cannot create torrentnotify.log\n")
		return 2
	}
	defer logger.Close()
	logwriter := io.MultiWriter(os.Stdout, logger)
	log.SetOutput(logwriter)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("%s started at %s\n", AppVersion, time.Now().Format(time.RFC3339))

	rpcURL, err := url.Parse(args.RpcURL)
	if err != nil {
		os.Stderr.WriteString("bad rpc url\n")
		return 2
	}
	advconf := &transmissionrpc.AdvancedConfig{
		HTTPS: false,
		Port:  9091,
	}
	if rpcURL.Scheme == "https" {
		advconf.HTTPS = true
	}
	if len(rpcURL.Path) > 0 && rpcURL.Path != "/" {
		advconf.RPCURI = rpcURL.Path
	}
	rpcpsw, _ := rpcURL.User.Password()
	transmissionbt, err := transmissionrpc.New(rpcURL.Hostname(), rpcURL.User.Username(), rpcpsw, advconf)
	if err != nil {
		log.Println(err)
		return 1
	}
	ok, serverVersion, serverMinimumVersion, err := transmissionbt.RPCVersion()
	if err != nil {
		log.Println(err)
		return 1
	}
	if !ok {
		log.Printf("Remote transmission RPC version (v%d) is incompatible: remote needs at least v%d", serverVersion, serverMinimumVersion)
		return 1
	}
	log.Printf("Remote transmission RPC version (v%d) is compatible\n", serverVersion)

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
			return
		}
		logger.Sync()
		b, err := ioutil.ReadFile("torrentnotify.log")
		if err == nil {
			w.Write(b)
		}
	})

	done := make(chan bool)
	wg := &sync.WaitGroup{}

	onShutdown(func() {

		close(done)
		wg.Wait()

		log.Printf("close signal received at %s\n", time.Now().Format(time.RFC3339))

		logger.Close()
		log.Println("logger closed")
		os.Exit(1)
	})

	wdrs := strings.Split(args.WatchDirs, ";")
	for _, wtchr := range wdrs {

		dir := strings.TrimSpace(wtchr)

		dw, err := dirwatch.New(dir)
		if err != nil {
			log.Printf("error watching torrent dir: %s\n", err)
		} else {
			log.Printf("watching torrent dir: %s\n", dir)

			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-done:
						return
					case ev := <-dw.Events:
						switch ev.Change {
						case dirwatch.Added:
							if !strings.HasSuffix(ev.TorrentFilePath, ".torrent") {
								continue
							}
							wg.Add(1)
							go addt(transmissionbt, ev.TorrentFilePath, wg, done)
						}
					}
				}
			}()
		}
	}

	http.ListenAndServe(args.ListenStat.String(), nil)
	return 1
}

func addt(client *transmissionrpc.Client, evfn string, wg *sync.WaitGroup, done chan bool) {
	defer wg.Done()
	defer func() {
		if ex := recover(); ex != nil {
			log.Println(ex)
		}
	}()

	<-time.After(2 * time.Second)
	if len(evfn) > 0 {
		log.Printf("adding %s", evfn)

		t, err := torrentAddFile(client, evfn)
		if err != nil {
			log.Printf("error adding torrent %s to transmission-daemon: %s\n", evfn, err)
			return
		}

		log.Printf("torrent added: [%d] %s, hash: %s\n", *t.ID, *t.Name, *t.HashString)

		os.Remove(evfn)

		// err = client.TorrentStartNowHashes([]string{*t.HashString})
		// if err != nil {
		// 	log.Printf("error starting torrent %s: %s\n", *t.Name, err)
		// 	return
		// }

		const SLEEP_INTERVAL = 5 * time.Second
		tck := time.NewTicker(SLEEP_INTERVAL)
		defer tck.Stop()
		for {
			select {
			case <-done:
				log.Printf("close monitoring of %s\n", *t.Name)
				return
			case <-tck.C:
				torrents, err := client.TorrentGet([]string{"percentDone"}, []int64{*t.ID})
				if err != nil {
					log.Printf("cannot retrieve status of %s: %s\n", *t.Name, err)
					return
				}
				for _, torrent := range torrents {
					log.Printf("percent done of %s: %0.1f\n", *t.Name, (*torrent.PercentDone)*100)
					if *torrent.PercentDone >= 1.0 {

						log.Printf("torrent is complete: %s\n", *t.Name)

						<-time.After(time.Duration(int64(args.AliveMinutes) * int64(time.Minute)))

						err = client.TorrentStopHashes([]string{*t.HashString})
						if err != nil {
							log.Printf("error stopping torrent %s: %s\n", *t.Name, err)
							return
						}
						err = client.TorrentRemove(&transmissionrpc.TorrentRemovePayload{DeleteLocalData: false, IDs: []int64{*t.ID}})
						if err != nil {
							log.Printf("error removing torrent %s: %s\n", *t.Name, err)
							return
						}

						return
					}
				}

			}
		}

	}
}

// func file2Base64(filename string) (b64 string, err error) {
// 	// Try to open file
// 	file, err := os.Open(filename)
// 	if err != nil {
// 		err = fmt.Errorf("open error: %v", err)
// 		return
// 	}
// 	defer file.Close()
// 	// Prepare encoder
// 	buffer := new(bytes.Buffer)
// 	encoder := base64.NewEncoder(base64.StdEncoding, buffer)
// 	defer encoder.Close()
// 	// Read file & encode
// 	if _, err = io.Copy(encoder, file); err != nil {
// 		err = fmt.Errorf("can't copy file content into the base64 encoder: %v", err)
// 	}
// 	// Read it
// 	b64 = buffer.String()
// 	return
// }

func torrentAddFile(c *transmissionrpc.Client, filename string) (torrent *transmissionrpc.Torrent, err error) {
	// Validate
	if filename == "" {
		err = errors.New("filename can't be empty")
		return
	}

	// b64, err := file2Base64(filename)
	// if err != nil {
	// 	err = fmt.Errorf("can't encode '%s' content as base64: %v", filename, err)
	// 	return
	// }
	fls := false
	dir := filepath.Dir(filename)
	return c.TorrentAdd(&transmissionrpc.TorrentAddPayload{
		// MetaInfo:    &b64,
		Filename:    &filename,
		Paused:      &fls,
		DownloadDir: &dir,
	})
}
