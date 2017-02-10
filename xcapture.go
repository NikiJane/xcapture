package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"time"
	"unsafe"

	"honnef.co/go/matroska"
	"honnef.co/go/matroska/ebml"
	"honnef.co/go/xcapture/internal/shm"

	"github.com/BurntSushi/xgb/composite"
	xshm "github.com/BurntSushi/xgb/shm"
	"github.com/BurntSushi/xgb/xfixes"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/xevent"
)

const bytesPerPixel = 4

type Buffer struct {
	Width  int
	Height int
	Pages  int
	Data   []byte
	ShmID  int
}

func (b Buffer) PageOffset(idx int) int {
	return b.PageSize() * idx
}

func (b Buffer) PageSize() int {
	return b.Width * b.Height * bytesPerPixel
}

func (b Buffer) Page(idx int) []byte {
	offset := b.PageOffset(idx)
	size := b.PageSize()
	return b.Data[offset : offset+size : offset+size]
}

type BitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   [4]byte
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

func NewBuffer(width, height, pages int) (Buffer, error) {
	size := width * height * pages * bytesPerPixel
	seg, err := shm.Create(size)
	if err != nil {
		return Buffer{}, err
	}
	data, err := seg.Attach()
	if err != nil {
		return Buffer{}, err
	}
	sh := &reflect.SliceHeader{
		Data: uintptr(data),
		Len:  size,
		Cap:  size,
	}
	b := (*(*[]byte)(unsafe.Pointer(sh)))
	return Buffer{
		Width:  width,
		Height: height,
		Pages:  pages,
		Data:   b,
		ShmID:  seg.ID,
	}, nil
}

func main() {
	fps := flag.Uint("fps", 60, "FPS")
	win := flag.Uint("win", 0, "Window ID")
	flag.Parse()

	xu, err := xgbutil.NewConn()
	if err != nil {
		log.Fatal("Couldn't connect to X server:", err)
	}
	if err := composite.Init(xu.Conn()); err != nil {
		log.Fatal("COMPOSITE extension is not available:", err)
	}
	if err := xfixes.Init(xu.Conn()); err != nil {
		log.Fatal("XFIXES extension is not available:", err)
	}
	xfixes.QueryVersion(xu.Conn(), 1, 0)
	if err := xshm.Init(xu.Conn()); err != nil {
		// TODO(dh) implement a slower version that is not using SHM
		log.Fatal("MIT-SHM extension is not available:", err)
	}
	if err := composite.RedirectWindowChecked(xu.Conn(), xproto.Window(*win), composite.RedirectAutomatic).Check(); err != nil {
		if err, ok := err.(xproto.AccessError); ok {
			log.Fatal("Can't capture window, another program seems to be capturing it already:", err)
		}
		log.Fatal("Can't capture window:", err)
	}
	pix, err := xproto.NewPixmapId(xu.Conn())
	if err != nil {
		log.Fatal("Could not obtain ID for pixmap:", err)
	}
	composite.NameWindowPixmap(xu.Conn(), xproto.Window(*win), pix)

	segID, err := xshm.NewSegId(xu.Conn())
	if err != nil {
		log.Fatal("Could not obtain ID for SHM:", err)
	}

	geom, err := xproto.GetGeometry(xu.Conn(), xproto.Drawable(*win)).Reply()
	if err != nil {
		log.Fatal("Could not determine window dimensions:", err)
	}
	width := geom.Width
	height := geom.Height

	buf, err := NewBuffer(int(width), int(height), 2)
	if err != nil {
		log.Fatal("Could not create shared memory:", err)
	}
	if err := xshm.AttachChecked(xu.Conn(), segID, uint32(buf.ShmID), false).Check(); err != nil {
		log.Fatal("Could not attach shared memory to X server:", err)
	}

	i := 0
	ch := make(chan []byte)

	bmp := BitmapInfoHeader{
		Width:    int32(width),
		Height:   int32(-height),
		Planes:   1,
		BitCount: 32,
	}
	codec := &bytes.Buffer{}
	if err := binary.Write(codec, binary.LittleEndian, bmp); err != nil {
		panic(err)
	}

	e := ebml.NewEncoder(os.Stdout)
	e.Emit(
		ebml.EBML(
			ebml.DocType(ebml.String("matroska")),
			ebml.DocTypeVersion(ebml.Uint(4)),
			ebml.DocTypeReadVersion(ebml.Uint(1))))

	e.EmitHeader(matroska.Segment, -1)
	e.Emit(
		matroska.Info(
			matroska.TimecodeScale(ebml.Uint(1)),
			matroska.MuxingApp(ebml.UTF8("honnef.co/go/mkv")),
			matroska.WritingApp(ebml.UTF8("xcapture"))))

	e.Emit(
		matroska.Tracks(
			matroska.TrackEntry(
				matroska.TrackNumber(ebml.Uint(1)),
				matroska.TrackUID(ebml.Uint(0xDEADBEEF)),
				matroska.TrackType(ebml.Uint(1)),
				matroska.FlagLacing(ebml.Uint(0)),
				matroska.DefaultDuration(ebml.Uint(time.Second/time.Duration(*fps))),
				matroska.CodecID(ebml.String("V_MS/VFW/FOURCC")),
				matroska.CodecPrivate(ebml.Binary(codec.Bytes())),
				matroska.Video(
					matroska.PixelWidth(ebml.Uint(width)),
					matroska.PixelHeight(ebml.Uint(height)),
					matroska.ColourSpace(ebml.Binary("BGRA")),
					matroska.Colour(
						matroska.BitsPerChannel(ebml.Uint(8)))))))

	go xevent.Main(xu)

	configureEvents := make(chan xevent.ConfigureNotifyEvent, 1e4)
	configCb := func(xu *xgbutil.XUtil, ev xevent.ConfigureNotifyEvent) {
		configureEvents <- ev
	}
	xevent.ConfigureNotifyFun(configCb).Connect(xu, xproto.Window(*win))
	err = xproto.ChangeWindowAttributesChecked(xu.Conn(), xproto.Window(*win),
		xproto.CwEventMask, []uint32{uint32(xproto.EventMaskStructureNotify)}).Check()
	if err != nil {
		log.Fatal("Couldn't monitor window for size changes:", err)
	}

	idx := -1
	var prevFrame []byte
	sendFrame := func(b []byte) {
		idx++
		if b == nil {
			b = prevFrame
		}
		prevFrame = b
		block := []byte{
			129,
			0, 0,
			128,
		}
		block = append(block, b...)
		e.Emit(
			matroska.Cluster(
				matroska.Timecode(ebml.Uint(idx*int(time.Second/time.Duration(*fps)))),
				matroska.Position(ebml.Uint(0)),
				matroska.SimpleBlock(ebml.Binary(block))))

		if e.Err != nil {
			log.Fatal(err)
		}
	}

	go func() {
		d := time.Second / time.Duration(*fps)
		t := time.NewTicker(d)
		pts := time.Now()
		dropped := 0
		for ts := range t.C {
			fps := float64(time.Second) / float64(ts.Sub(pts))
			// XXX we are racing on width and height
			fmt.Fprintf(os.Stderr, "\rFrame time: %14s (%4.2f FPS); %5d dropped; %4dx%4d -> %4dx%4d          ", ts.Sub(pts), fps, dropped, width, height, buf.Width, buf.Height)
			pts = ts
			select {
			case b := <-ch:
				sendFrame(b)
			default:
				dropped++
				sendFrame(nil)
			}
		}
	}()

	scratch := make([]byte, buf.PageSize())
	for {
		select {
		case ev := <-configureEvents:
			if ev.Width != width || ev.Height != height {
				width = ev.Width
				height = ev.Height

			}
			// DRY
			xproto.FreePixmap(xu.Conn(), pix)
			var err error
			pix, err = xproto.NewPixmapId(xu.Conn())
			if err != nil {
				log.Fatal("Could not obtain ID for pixmap:", err)
			}
			composite.NameWindowPixmap(xu.Conn(), xproto.Window(*win), pix)
		default:
			offset := buf.PageOffset(i)
			w := width
			if int(w) > buf.Width {
				w = uint16(buf.Width)
			}
			h := height
			if int(h) > buf.Height {
				h = uint16(buf.Height)
			}
			_, err := xshm.GetImage(xu.Conn(), xproto.Drawable(pix), 0, 0, w, h, 0xFFFFFFFF, xproto.ImageFormatZPixmap, segID, uint32(offset)).Reply()
			if err != nil {
				log.Println("Could not fetch window contents:", err)
				continue
			}

			page := buf.Page(i)

			// TODO(dh): instead of copying into scratch and back, we
			// should have a third page that we can copy into and send
			// directly onto the channel
			if int(w) < buf.Width || int(h) < buf.Height {
				copy(scratch, page)
				for i := range page {
					page[i] = 0
				}
				for i := 0; i < int(h); i++ {
					copy(page[i*buf.Width*bytesPerPixel:], scratch[i*int(w)*bytesPerPixel:(i+1)*int(w)*bytesPerPixel])
				}
			}

			drawCursor(xu, *win, buf, page)

			ch <- page
			i = (i + 1) % 2
		}
	}
}

func drawCursor(xu *xgbutil.XUtil, win uint, buf Buffer, page []byte) {
	cursor, err := xfixes.GetCursorImage(xu.Conn()).Reply()
	if err != nil {
		return
	}
	pos, err := xproto.TranslateCoordinates(xu.Conn(), xu.RootWin(), xproto.Window(win), cursor.X, cursor.Y).Reply()
	if err != nil {
		return
	}
	if pos.DstY < 0 || pos.DstX < 0 || int(pos.DstY) > buf.Height || int(pos.DstX) > buf.Width {
		// cursor outside of our window
		return
	}
	for i, p := range cursor.CursorImage {
		row := i/int(cursor.Width) + int(pos.DstY) - int(cursor.Yhot)
		col := i%int(cursor.Width) + int(pos.DstX) - int(cursor.Xhot)
		if row >= buf.Height || col >= buf.Width || row < 0 || col < 0 {
			// cursor is partially off-screen
			break
		}
		off := row*buf.Width*bytesPerPixel + col*bytesPerPixel
		alpha := (p >> 24) + 1
		invAlpha := uint32(256 - (p >> 24))

		page[off+3] = 255
		page[off+2] = byte((alpha*uint32(byte(p>>16)) + invAlpha*uint32(page[off+2])) >> 8)
		page[off+1] = byte((alpha*uint32(byte(p>>8)) + invAlpha*uint32(page[off+1])) >> 8)
		page[off+0] = byte((alpha*uint32(byte(p>>0)) + invAlpha*uint32(page[off+0])) >> 8)
	}
}
