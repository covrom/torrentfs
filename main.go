package main

import (
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/anacrolix/envpprof"
	"github.com/anacrolix/missinggo/slices"
	"github.com/anacrolix/tagflag"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/anacrolix/torrent/util/dirwatch"
	"github.com/covrom/torrentfs/store"
	humanize "github.com/dustin/go-humanize"
	"golang.org/x/time/rate"
)

const AppVersion = "torrentfs 1.1"

var (
	args = struct {
		WatchDirs string `help:"torrent files locations separated by semicolon"`

		BannedFile     string        `help:"banned ip list"`
		UploadRate     tagflag.Bytes `help:"max piece bytes to send per second"`
		DownloadRate   tagflag.Bytes `help:"max bytes per second down from peers"`
		ListenAddr     *net.TCPAddr
		ListenStat     *net.TCPAddr
		AliveMinutes   int
		ActiveTorrents int
		Version        bool
	}{
		ListenAddr:     &net.TCPAddr{Port: 16881},
		ListenStat:     &net.TCPAddr{Port: 8800},
		Version:        false,
		BannedFile:     "block.ip.list",
		AliveMinutes:   240,
		ActiveTorrents: 10,
		DownloadRate:   -1,
		UploadRate:     1024 * 1024 / 8,
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

	logger, err := os.Create("torrentfs.log")
	if err != nil {
		os.Stderr.WriteString("cannot create torrentfs.log\n")
		return 2
	}
	defer logger.Close()
	logwriter := io.MultiWriter(os.Stdout, logger)
	log.SetOutput(logwriter)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Printf("%s started at %s\n", AppVersion, time.Now().Format(time.RFC3339))
	cfg := torrent.NewDefaultClientConfig()

	cfg.DataDir = ""
	cfg.Seed = true
	cfg.DisableTrackers = false
	cfg.DisableIPv6 = true
	blocklist, err := iplist.MMapPackedFile(args.BannedFile)
	if err == nil {
		defer blocklist.Close()
		cfg.IPBlocklist = blocklist
	}
	cfg.DefaultStorage = storage.NewMMap("")
	if args.UploadRate != -1 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(args.UploadRate), 256<<10)
	}
	if args.DownloadRate != -1 {
		cfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(args.DownloadRate), 1<<20)
	}
	cfg.SetListenAddr(args.ListenAddr.String())

	client, err := torrent.NewClient(cfg)
	if err != nil {
		log.Println(err)
		return 1
	}
	defer client.Close()

	type htmlTt struct {
		Name      string
		Completed string
		Total     string
		Seeds     int
		Hash      string
	}

	tpl := template.Must(template.New("index.html").Parse(`<!DOCTYPE html>
<html>

<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<meta http-equiv="X-UA-Compatible" content="ie=edge">
	<title>torrentfs</title>
</head>

<body>
	<a href="/stat">Full status</a> <a href="/log">Current log</a> <br/>
	{{range .}}
	<pre>{{.Name}} {{.Completed}}/{{.Total}} Seed:{{.Seeds}} <a href="/del?hash={{.Hash}}">Delete</a> </pre>
	{{end}}
</body>

</html>
`))

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
			return
		}
		tt := client.Torrents()
		hts := make([]htmlTt, len(tt))
		for i, t := range tt {
			hts[i] = htmlTt{
				Name:      t.Name(),
				Seeds:     t.Stats().ConnectedSeeders,
				Completed: humanize.Bytes(uint64(t.BytesCompleted())),
				Total:     humanize.Bytes(uint64(t.Info().TotalLength())),
				Hash:      t.InfoHash().String(),
			}
		}
		err := tpl.ExecuteTemplate(w, "index.html", hts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	http.HandleFunc("/stat", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
			return
		}
		client.WriteStatus(w)
	})

	http.HandleFunc("/log", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
			return
		}
		logger.Sync()
		// w.Write([]byte("\n\nCurrent log:\n\n"))
		b, err := ioutil.ReadFile("torrentfs.log")
		if err == nil {
			w.Write(b)
		}
	})

	http.HandleFunc("/del", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
			return
		}
		hs := req.FormValue("hash")
		tt := client.Torrents()
		for _, t := range tt {
			if t.InfoHash().String() == hs {
				t.Drop()
				break
			}
		}
		http.Redirect(w, req, "/", http.StatusSeeOther)
	})

	chq := make(chan *torrent.Torrent, args.ActiveTorrents)
	done := make(chan bool)
	wg := &sync.WaitGroup{}

	onShutdown(func() {
		close(done)
		wg.Wait()

		log.Printf("close signal received at %s\n", time.Now().Format(time.RFC3339))
		client.Close()
		log.Println("client closed")

		logger.Close()
		log.Println("logger closed")
		os.Exit(1)
	})

	wg.Add(args.ActiveTorrents)

	for i := 0; i < args.ActiveTorrents; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				case tt := <-chq:
					fn := tt.Name()
					<-tt.GotInfo()
					tt.DownloadAll()
					tck := time.NewTicker(5 * time.Second)
					ttcl := tt.Closed()
				loop:
					for {
						select {
						case <-ttcl:
							log.Printf("closed %s\n", fn)
							tck.Stop()
							return
						case <-tt.GotInfo():
							if tt.BytesCompleted() == tt.Info().TotalLength() {
								tck.Stop()
								break loop
							} else {
								log.Printf("downloading (%s/%s) %s", humanize.Bytes(uint64(tt.BytesCompleted())), humanize.Bytes(uint64(tt.Info().TotalLength())), fn)
							}
							select {
							case <-tck.C:
							case <-done:
								tck.Stop()
								tt.Drop()
								log.Printf("drop %s\n", fn)
								return
							}
						case <-done:
							tck.Stop()
							tt.Drop()
							log.Printf("drop %s\n", fn)
							return
						}
					}
					log.Printf("torrent is complete %s", fn)
					select {
					case <-done:
						tt.Drop()
						log.Printf("drop %s\n", fn)
						return
					case <-time.After(time.Duration(int64(args.AliveMinutes) * int64(time.Minute))):
					}
					tt.Drop()
					log.Printf("drop %s\n", fn)
				}
			}
		}()
	}

	wdrs := strings.Split(args.WatchDirs, ";")
	for _, wtchr := range wdrs {

		dir := strings.TrimSpace(wtchr)

		storageImpl := store.NewFile(dir)
		defer storageImpl.Close()

		dw, err := dirwatch.New(dir)
		if err != nil {
			log.Printf("error watching torrent dir: %s\n", err)
		} else {
			log.Printf("watching torrent dir: %s\n", dir)

			wg.Add(1)
			go func(sti storage.ClientImpl) {
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
							go func(evfn string) {
								defer wg.Done()
								<-time.After(2 * time.Second)
								log.Printf("adding %s", evfn)
								if len(evfn) > 0 {
									mi, err := metainfo.LoadFromFile(evfn)
									if err != nil {
										log.Printf("error adding torrent %s to client: %s\n", evfn, err)
									} else {
										spec := torrent.TorrentSpecFromMetaInfo(mi)

										spec.Storage = sti

										t, _, err := client.AddTorrentSpec(spec)
										var ss []string
										slices.MakeInto(&ss, mi.Nodes)
										client.AddDHTNodes(ss)

										if err != nil {
											log.Printf("error adding torrent %s to client: %s\n", evfn, err)
										} else {
											wg.Add(1)
											go func(tt *torrent.Torrent, fn string) {
												defer wg.Done()
												select {
												case chq <- tt:
													log.Printf("delete file %s", fn)
													os.Remove(fn)
												case <-done:
												}
											}(t, evfn)
										}
									}
								}
							}(ev.TorrentFilePath)
						}
					}
				}
			}(storageImpl)
		}
	}

	http.ListenAndServe(args.ListenStat.String(), nil)
	return 1
}
