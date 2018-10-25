package store

import (
	"io"
	"log"
	"os"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type filePieceImpl struct {
	*fileTorrentImpl
	p metainfo.Piece
	io.WriterAt
	io.ReaderAt
}

var _ storage.PieceImpl = (*filePieceImpl)(nil)

func (me *filePieceImpl) pieceKey() metainfo.PieceKey {
	return metainfo.PieceKey{me.infoHash, me.p.Index()}
}

func (fs *filePieceImpl) Completion() storage.Completion {
	c, err := fs.completion.Get(fs.pieceKey())
	if err != nil {
		log.Printf("error getting piece completion: %s", err)
		c.Ok = false
		return c
	}
	// If it's allegedly complete, check that its constituent files have the
	// necessary length.
	for _, fi := range extentCompleteRequiredLengths(fs.p.Info, fs.p.Offset(), fs.p.Length()) {
		s, err := os.Stat(fs.fileInfoName(fi))
		if err != nil || s.Size() < fi.Length {
			c.Complete = false
			break
		}
	}
	if !c.Complete {
		// The completion was wrong, fix it.
		fs.completion.Set(fs.pieceKey(), false)
	}
	return c
}

func (fs *filePieceImpl) MarkComplete() error {
	return fs.completion.Set(fs.pieceKey(), true)
}

func (fs *filePieceImpl) MarkNotComplete() error {
	return fs.completion.Set(fs.pieceKey(), false)
}

func extentCompleteRequiredLengths(info *metainfo.Info, off, n int64) (ret []metainfo.FileInfo) {
	if n == 0 {
		return
	}
	for _, fi := range info.UpvertedFiles() {
		if off >= fi.Length {
			off -= fi.Length
			continue
		}
		n1 := n
		if off+n1 > fi.Length {
			n1 = fi.Length - off
		}
		ret = append(ret, metainfo.FileInfo{
			Path:   fi.Path,
			Length: off + n1,
		})
		n -= n1
		if n == 0 {
			return
		}
		off = 0
	}
	panic("extent exceeds torrent bounds")
}
