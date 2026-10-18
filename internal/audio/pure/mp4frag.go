package pure

import (
	"encoding/binary"
	"errors"
	"io"
)

// fragmentedMP4 reads a file that keeps its samples in a run of moof and mdat
// pairs rather than in the sample tables a progressive file carries. The header
// still describes the track, so it is read the same way; only where the samples
// are, and how long each one is, comes from somewhere else.
//
// The whole file is walked once to collect the samples, which a progressive file
// does not need. That is the price of a format built for streaming: nothing says
// where the samples are until the fragment that holds them has been read.
func fragmentedMP4(rs io.ReadSeeker, moov []byte) (*mp4Audio, error) {
	trak := audioTrack(moov)
	if trak == nil {
		return nil, errNoAudioTrack
	}
	mdia := findBox(trak, "mdia")
	desc := esdsConfig(findBox(findBox(mdia, "minf"), "stbl"))
	if len(desc) == 0 {
		return nil, errNoASC
	}
	id := trackID(trak)
	if id == 0 {
		return nil, errNoAudioTrack
	}

	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var sizes []uint32
	var offsets []int64
	var at int64
	for {
		typ, size, header, err := readBox(rs)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		start := at
		at += size
		body := size - header
		if typ == "moof" {
			if body > maxHeader {
				return nil, errHeaderTooBig
			}
			buf := make([]byte, body)
			if _, err := io.ReadFull(rs, buf); err != nil {
				return nil, err
			}
			s, o, err := fragmentSamples(buf, start, id)
			if err != nil {
				return nil, err
			}
			sizes = append(sizes, s...)
			offsets = append(offsets, o...)
			if len(sizes) > maxSamples {
				return nil, errNoSamples
			}
			continue
		}
		if _, err := rs.Seek(body, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
	if len(sizes) == 0 {
		return nil, errNoSamples
	}
	// The reader consumes the file in order, so it starts on the first sample
	// rather than at the beginning.
	if _, err := rs.Seek(offsets[0], io.SeekStart); err != nil {
		return nil, err
	}
	return &mp4Audio{
		r:        rs,
		consumed: offsets[0],
		sizes:    sizes,
		offsets:  offsets,
		desc:     desc,
		priming:  editPriming(trak, mdia, desc),
	}, nil
}

// fragmentSamples reads the sizes and file offsets of the samples in one moof.
// A fragment names the track it belongs to and gives the samples' sizes in a run
// table; the bytes themselves are in the mdat that follows.
func fragmentSamples(moof []byte, at int64, id uint32) ([]uint32, []int64, error) {
	var sizes []uint32
	var offsets []int64
	for _, traf := range boxesOf(moof, "traf") {
		tfhd := findBox(traf, "tfhd")
		if len(tfhd) < 8 {
			continue
		}
		flags := binary.BigEndian.Uint32(tfhd[0:4]) & 0xFFFFFF
		if binary.BigEndian.Uint32(tfhd[4:8]) != id {
			continue
		}
		// A fragment with no base of its own is based on the box that holds it,
		// which is what the default base is moof flag asks for.
		base := at
		var defaultSize uint32
		p := 8
		if flags&0x000001 != 0 {
			if len(tfhd) < p+8 {
				return nil, nil, errNoSamples
			}
			base = int64(binary.BigEndian.Uint64(tfhd[p : p+8]))
			p += 8
		}
		if flags&0x000002 != 0 {
			p += 4
		}
		if flags&0x000008 != 0 {
			p += 4
		}
		if flags&0x000010 != 0 {
			if len(tfhd) < p+4 {
				return nil, nil, errNoSamples
			}
			defaultSize = binary.BigEndian.Uint32(tfhd[p : p+4])
			p += 4
		}
		if flags&0x000020 != 0 {
			p += 4
		}
		for _, trun := range boxesOf(traf, "trun") {
			s, o, err := runSamples(trun, base, defaultSize)
			if err != nil {
				return nil, nil, err
			}
			sizes = append(sizes, s...)
			offsets = append(offsets, o...)
		}
	}
	return sizes, offsets, nil
}

// runSamples reads one run table. The per sample fields are all four bytes wide
// and appear in flag order, so the cursor walks them in that order whether or not
// this reader wants the value it passes.
func runSamples(trun []byte, base int64, defaultSize uint32) ([]uint32, []int64, error) {
	if len(trun) < 8 {
		return nil, nil, errNoSamples
	}
	flags := binary.BigEndian.Uint32(trun[0:4]) & 0xFFFFFF
	count := int(binary.BigEndian.Uint32(trun[4:8]))
	if count <= 0 || count > maxSamples {
		return nil, nil, errNoSamples
	}
	p := 8
	if flags&0x000001 == 0 {
		// Without a data offset the samples follow whatever came before them,
		// which would mean tracking the previous run across the whole file. The
		// fall back covers those.
		return nil, nil, errNoSamples
	}
	if len(trun) < p+4 {
		return nil, nil, errNoSamples
	}
	next := base + int64(int32(binary.BigEndian.Uint32(trun[p:p+4])))
	p += 4
	if flags&0x000004 != 0 {
		p += 4
	}
	sizes := make([]uint32, 0, count)
	offsets := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		size := defaultSize
		// The fields this reader does not want are still walked over, so a table
		// carrying them is read at the right place; a table that claims more than
		// it holds is refused rather than read past.
		if flags&0x000100 != 0 {
			if len(trun) < p+4 {
				return nil, nil, errNoSamples
			}
			p += 4
		}
		if flags&0x000200 != 0 {
			if len(trun) < p+4 {
				return nil, nil, errNoSamples
			}
			size = binary.BigEndian.Uint32(trun[p : p+4])
			p += 4
		}
		if flags&0x000400 != 0 {
			if len(trun) < p+4 {
				return nil, nil, errNoSamples
			}
			p += 4
		}
		if flags&0x000800 != 0 {
			if len(trun) < p+4 {
				return nil, nil, errNoSamples
			}
			p += 4
		}
		sizes = append(sizes, size)
		offsets = append(offsets, next)
		next += int64(size)
	}
	return sizes, offsets, nil
}
