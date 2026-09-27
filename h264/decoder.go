// Package h264 decodes H.264 with the openh264 library that mediadevices
// bundles for its encoder. The library is linked by the mediadevices openh264
// package; only its headers live here, in include/, and must match it.
package h264

/*
#cgo CFLAGS: -I${SRCDIR}/include
#include <string.h>
#include "codec_api.h"

static ISVCDecoder* decoder_new(void) {
	ISVCDecoder* d = NULL;
	if (WelsCreateDecoder(&d) != 0 || d == NULL) {
		return NULL;
	}
	SDecodingParam p;
	memset(&p, 0, sizeof p);
	p.sVideoProperty.eVideoBsType = VIDEO_BITSTREAM_AVC;
	if ((*d)->Initialize(d, &p) != 0) {
		WelsDestroyDecoder(d);
		return NULL;
	}
	return d;
}

typedef struct {
	int state, ready, width, height, strideY, strideUV;
	unsigned char *y, *u, *v;
} picture;

static picture decoder_decode(ISVCDecoder* d, unsigned char* buf, int n) {
	picture pic;
	unsigned char* planes[3] = {0};
	SBufferInfo info;
	memset(&pic, 0, sizeof pic);
	memset(&info, 0, sizeof info);
	pic.state = (*d)->DecodeFrameNoDelay(d, buf, n, planes, &info);
	if (info.iBufferStatus == 1) {
		pic.ready = 1;
		pic.width = info.UsrData.sSystemBuffer.iWidth;
		pic.height = info.UsrData.sSystemBuffer.iHeight;
		pic.strideY = info.UsrData.sSystemBuffer.iStride[0];
		pic.strideUV = info.UsrData.sSystemBuffer.iStride[1];
		pic.y = planes[0];
		pic.u = planes[1];
		pic.v = planes[2];
	}
	return pic;
}

static void decoder_free(ISVCDecoder* d) {
	(*d)->Uninitialize(d);
	WelsDestroyDecoder(d);
}
*/
import "C"

import (
	"errors"
	"image"
	"unsafe"

	_ "github.com/pion/mediadevices/pkg/codec/openh264"
)

// ErrDecode reports a frame the decoder could not make sense of, usually
// because a frame it depends on was lost. A key frame recovers from it.
var ErrDecode = errors.New("h264: decode error")

// Decoder turns H.264 access units into pictures. It is not safe for
// concurrent use.
type Decoder struct {
	dec *C.ISVCDecoder
	pic *image.YCbCr
}

func NewDecoder() (*Decoder, error) {
	dec := C.decoder_new()
	if dec == nil {
		return nil, errors.New("h264: cannot create decoder")
	}
	return &Decoder{dec: dec}, nil
}

// Decode takes one access unit in Annex B form. It returns the picture, or
// nil when there is none to show yet. The picture is reused: it stays valid
// only until the next call.
func (d *Decoder) Decode(au []byte) (*image.YCbCr, error) {
	if len(au) == 0 {
		return nil, nil
	}
	pic := C.decoder_decode(d.dec, (*C.uchar)(unsafe.Pointer(&au[0])), C.int(len(au)))
	if pic.ready == 0 {
		if pic.state != 0 {
			return nil, ErrDecode
		}
		return nil, nil
	}
	return d.copyPicture(pic), nil
}

// copyPicture moves the decoder's planes, which it reuses, into d.pic.
func (d *Decoder) copyPicture(pic C.picture) *image.YCbCr {
	w, h := int(pic.width), int(pic.height)
	if d.pic == nil || d.pic.Rect.Dx() != w || d.pic.Rect.Dy() != h {
		d.pic = image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	}
	copyPlane(d.pic.Y, d.pic.YStride, pic.y, int(pic.strideY), w, h)
	cw, ch := (w+1)/2, (h+1)/2
	copyPlane(d.pic.Cb, d.pic.CStride, pic.u, int(pic.strideUV), cw, ch)
	copyPlane(d.pic.Cr, d.pic.CStride, pic.v, int(pic.strideUV), cw, ch)
	return d.pic
}

func copyPlane(dst []byte, dstStride int, src *C.uchar, srcStride, w, h int) {
	for row := range h {
		line := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(src), row*srcStride)), w)
		copy(dst[row*dstStride:], line)
	}
}

func (d *Decoder) Close() {
	if d.dec != nil {
		C.decoder_free(d.dec)
		d.dec = nil
	}
}
