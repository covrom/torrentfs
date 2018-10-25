package main

import (
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

		BannedFile     string        `help:"banned ip list"`
		UploadRate     tagflag.Bytes `help:"max piece bytes to send per second"`
		DownloadRate   tagflag.Bytes `help:"max bytes per second down from peers"`
		ReadaheadBytes tagflag.Bytes
		ListenAddr     *net.TCPAddr
		ListenStat     *net.TCPAddr
		AliveMinutes   tagflag.Bytes
		Version        bool
	}{
		ReadaheadBytes: 10 << 20,
		ListenAddr:     &net.TCPAddr{},
		ListenStat:     &net.TCPAddr{Port: 8800},
		Version:        false,
		BannedFile:     "block.ip.list",
		UploadRate:     -1,
		DownloadRate:   -1,
		AliveMinutes:   240,
	}
)

func exitSignalHandlers(client *torrent.Client) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	for {
		log.Printf("close signal received: %+v", <-c)
		client.Close()
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
	log.SetFlags(log.LstdFlags | log.Lshortfile)

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
	go exitSignalHandlers(client)

	// Write status on the root path on the default HTTP muxer. This will be
	// bound to localhost somewhere if GOPPROF is set, thanks to the envpprof
	// import.
	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		client.WriteStatus(w)
	})

	wdrs := strings.Split(args.WatchDirs, ";")
	for _, wtchr := range wdrs {

		dir := strings.TrimSpace(wtchr)

		storageImpl := store.NewFile(dir)

		dw, err := dirwatch.New(wtchr)
		if err != nil {
			log.Printf("error watching torrent dir: %s\n", err)
		} else {

			go func(sti storage.ClientImpl) {
				for ev := range dw.Events {
					switch ev.Change {
					case dirwatch.Added:
						go func(evfn string) {
							<-time.After(2 * time.Second)
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

										os.Remove(evfn)

										go func(tt *torrent.Torrent, fn string) {
											<-tt.GotInfo()
											tt.DownloadAll()

											<-time.After(time.Duration(int64(args.AliveMinutes) * int64(time.Minute)))
											tt.Drop()
											log.Printf("torrent drop %s\n", fn)
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
