package main

import (
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
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
	"golang.org/x/time/rate"
)

const AppVersion = "torrentfs 1.0"

var (
	args = struct {
		WatchDirs string `help:"torrent files locations separated by semicolon"`

		BannedFile   string        `help:"banned ip list"`
		UploadRate   tagflag.Bytes `help:"max piece bytes to send per second"`
		DownloadRate tagflag.Bytes `help:"max bytes per second down from peers"`
		ListenAddr   *net.TCPAddr
		ListenStat   *net.TCPAddr
		AliveMinutes int
		Version      bool
	}{
		ListenAddr:   &net.TCPAddr{Port: 16881},
		ListenStat:   &net.TCPAddr{Port: 8800},
		Version:      false,
		BannedFile:   "block.ip.list",
		AliveMinutes: 240,
		DownloadRate: -1,
		UploadRate:   1024 * 1024 / 8,
	}
)

func exitSignalHandlers(client *torrent.Client, logger io.Closer) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	for {
		log.Printf("close signal received at %s: %+v\n", time.Now().Format(time.RFC3339), <-c)
		client.Close()
		logger.Close()
		os.Exit(0)
	}
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
	go exitSignalHandlers(client, logger)

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		client.WriteStatus(w)
		logger.Sync()
		w.Write([]byte("\n\nCurrent log:\n\n"))
		b, err := ioutil.ReadFile("torrentfs.log")
		if err == nil {
			w.Write(b)
		}
	})

	wdrs := strings.Split(args.WatchDirs, ";")
	for _, wtchr := range wdrs {

		dir := strings.TrimSpace(wtchr)

		storageImpl := store.NewFile(dir)
		defer storageImpl.Close()

		dw, err := dirwatch.New(wtchr)
		if err != nil {
			log.Printf("error watching torrent dir: %s\n", err)
		} else {

			go func(sti storage.ClientImpl) {
				for ev := range dw.Events {
					switch ev.Change {
					case dirwatch.Added:
						if !strings.HasSuffix(ev.TorrentFilePath, ".torrent") {
							continue
						}
						go func(evfn string) {
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
										log.Printf("delete file %s", evfn)
										os.Remove(evfn)

										go func(tt *torrent.Torrent, fn string) {
											<-tt.GotInfo()
											tt.DownloadAll()
											log.Printf("torrent is complete %s", fn)
											<-time.After(time.Duration(int64(args.AliveMinutes) * int64(time.Minute)))
											tt.Drop()
											log.Printf("drop %s\n", fn)
										}(t, evfn)
									}
								}
							}
						}(ev.TorrentFilePath)
					}
				}
			}(storageImpl)
		}
	}

	http.ListenAndServe(args.ListenStat.String(), nil)
	return 1
}
