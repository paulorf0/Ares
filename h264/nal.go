package h264

// IsKeyFrame reports whether an Annex B access unit holds an IDR slice, the
// kind of frame a decoder can start from.
func IsKeyFrame(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		// A start code is 00 00 01, possibly after one more zero byte.
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			if au[i+3]&0x1f == nalIDR {
				return true
			}
			i += 2
		}
	}
	return false
}

const nalIDR = 5
