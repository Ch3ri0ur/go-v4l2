//go:build linux
// +build linux

package v4l2

import (
	"encoding/json"
	"fmt"
	"io"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	defaultNumBuffers = 2
)

type Buffer struct {
	Data []byte

	fd    int
	index int
}

// Release buffer so that it may be re-enqueued into the device
func (b *Buffer) Release() error {
	return enqueue(b.fd, b.index)
}

type Device struct {
	C       chan Buffer
	buffers [][]byte
	fd      int
}

// Open device
func Open(path string) (*Device, error) {
	fmt.Println("OpenDevice")
	fd, err := unix.Open(path, unix.O_RDWR, 0666)
	if nil != err {
		return nil, err
	}

	return &Device{
		C:       make(chan Buffer, defaultNumBuffers),
		buffers: make([][]byte, defaultNumBuffers),
		fd:      fd,
	}, nil
}

// Close device
func (dev *Device) Close() error {
	return unix.Close(dev.fd)
}

// Rotate configures the picture rotation
func (dev *Device) SetRotation(bitrate int32) error {
	fmt.Println("SetRotation")
	return setUserControl(dev.fd, V4L2_CID_ROTATE, bitrate)
}

// SetBitrate configures the target bitrate of encoder
func (dev *Device) SetBitrate(bitrate int32) error {
	fmt.Println("SetBitrate")

	return setCodecControl(dev.fd, V4L2_CID_MPEG_VIDEO_BITRATE, bitrate)
}

// SetPixelFormat configures frame geometry and pixel format. The pixel
// format may be a compressor supported by the device, such as MJPEG or
// H.264.
func (dev *Device) SetPixelFormat(width, height, format int) error {
	pfmt := v4l2_pix_format{
		width:       uint32(width),
		height:      uint32(height),
		pixelformat: uint32(format),
		field:       V4L2_FIELD_ANY,
	}
	fmt.Println("SetPixelFormat_pfmt")
	fmt.Printf("%d %d %d\n", pfmt.width, pfmt.height, pfmt.pixelformat)
	st, _ := json.MarshalIndent(pfmt, "", "\t")
	fmt.Println(st)

	ft := v4l2_format{
		typ: V4L2_BUF_TYPE_VIDEO_CAPTURE,
		fmt: pfmt.marshal(),
	}
	fmt.Println("SetPixelFormat")
	s, _ := json.MarshalIndent(ft, "", "\t")
	fmt.Println(s)
	fmt.Printf("%#v\n", ft)

	return ioctl(dev.fd, VIDIOC_S_FMT, unsafe.Pointer(&ft))
}

// SetRepeatSequenceHeader configures the device to output sequence
// parameter sets (SPS) and picture parameter sets (PPS) before each
// group-of-pictures (GoP). This is H.264 specific and not supported by
// all devices.
func (dev *Device) SetRepeatSequenceHeader(on bool) error {
	var value int32
	if on {
		value = 1
	}
	fmt.Println("SetRepeatSequenceHeader")
	return setCodecControl(dev.fd, V4L2_CID_MPEG_VIDEO_REPEAT_SEQ_HEADER, value)
}

// Start video capture
func (dev *Device) Start() error {
	// Request specified number of kernel-space buffers from device
	if err := requestBuffers(dev.fd, len(dev.buffers)); nil != err {
		return err
	}

	// For each buffer...
	for i := 0; i < len(dev.buffers); i++ {
		// Get length and offset of i-th buffer
		length, offset, err := queryBuffer(dev.fd, uint32(i))
		if nil != err {
			return err
		}

		// Memory map i-th buffer to user-space
		if dev.buffers[i], err = unix.Mmap(
			dev.fd,
			int64(offset),
			int(length),
			unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_SHARED,
		); nil != err {
			return err
		}

		// Enqueue to device for population
		if err := enqueue(dev.fd, i); nil != err {
			return err
		}
	}

	go func(dev *Device) {
		for {
			// Dequeue buffer
			i, n, err := dequeue(dev.fd)
			if nil != err {
				if err == syscall.EINVAL {
					err = io.EOF
				}
				return
			}

			// Write buffer to channel
			// Note: Zero-copy. Slice bounds are written, not contents.
			dev.C <- Buffer{
				Data: dev.buffers[i][:n],

				fd:    dev.fd,
				index: i,
			}
		}
	}(dev)

	// Enable stream
	typ := V4L2_BUF_TYPE_VIDEO_CAPTURE
	fmt.Println("stream enable")
	return ioctl(dev.fd, VIDIOC_STREAMON, unsafe.Pointer(&typ))
}

// Stop video capture
func (dev *Device) Stop() error {
	fmt.Println("Stop video capture")
	// Disable stream (dequeues any outstanding buffers as well).
	typ := V4L2_BUF_TYPE_VIDEO_CAPTURE
	if err := ioctl(dev.fd, VIDIOC_STREAMOFF, unsafe.Pointer(&typ)); nil != err {
		return nil
	}

	// Unmap each buffer from user space
	for i := 0; i < len(dev.buffers); i++ {
		if dev.buffers[i] != nil {
			if err := unix.Munmap(dev.buffers[i]); err != nil {
				return err
			}
			dev.buffers[i] = nil
		}
	}

	// Count of zero frees all buffers, after aborting or finishing any
	// DMA in progress.
	return requestBuffers(dev.fd, 0)
}

// Allows Custom User Control Settings
func (dev *Device) SetCustomUserControl(id uint32, value int32) error {
	return setUserControl(dev.fd, id, value)
}

// setUserControl configures the value of a user-specific control id
func setUserControl(fd int, id uint32, value int32) error {
	fmt.Println("setUserControl")
	fmt.Printf("%d id: %d value: %d\n", V4L2_CTRL_CLASS_USER, id, value)

	return setControl(fd, V4L2_CTRL_CLASS_USER, id, value)
}

// Allows Custom Codec Control Settings
func (dev *Device) SetCustomCodecControl(id uint32, value int32) error {
	return setCodecControl(dev.fd, id, value)
}

// setCodecControl configures the value of a codec-specific control id
func setCodecControl(fd int, id uint32, value int32) error {
	fmt.Println("setCodecControl")
	fmt.Printf("%d id: %d value: %d\n", V4L2_CTRL_CLASS_MPEG, id, value)

	return setControl(fd, V4L2_CTRL_CLASS_MPEG, id, value)
}

// setControl configures the value of a control id
func setControl(fd int, class, id uint32, value int32) error {
	fmt.Println("setControl")

	const numControls = 1

	ctrls := [numControls]v4l2_ext_control{
		v4l2_ext_control{
			id:   id,
			size: 0,
		},
	}
	nativeEndian.PutUint32(ctrls[0].value[:], uint32(value))
	fmt.Println("SetControl_ctrls")
	fmt.Printf("%#v\n", ctrls)

	extctrls := v4l2_ext_controls{
		ctrl_class: class,
		count:      numControls,
		controls:   unsafe.Pointer(&ctrls),
	}
	fmt.Println("SetControl_ctrls2")
	fmt.Printf("%#v\n", extctrls)

	return ioctl(fd, VIDIOC_S_EXT_CTRLS, unsafe.Pointer(&extctrls))
}

// ioctl system call for device control
func ioctl(fd int, req uint, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(req),
		uintptr(arg),
	); errno != 0 {
		return errno
	}
	return nil
}

// Query buffer parameters.
func queryBuffer(fd int, n uint32) (length, offset uint32, err error) {
	fmt.Println("queryBuffer")
	qb := v4l2_buffer{
		index:  n,
		typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
		memory: V4L2_MEMORY_MMAP,
	}
	if err = ioctl(fd, VIDIOC_QUERYBUF, unsafe.Pointer(&qb)); err != nil {
		return
	}

	length = qb.length
	offset = nativeEndian.Uint32(qb.m[0:4])
	return
}

// Request specified number of kernel-space buffers from device
func requestBuffers(fd int, n int) error {
	fmt.Println("requestBuffers")

	rb := v4l2_requestbuffers{
		count:  uint32(n),
		typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
		memory: V4L2_MEMORY_MMAP,
	}
	return ioctl(fd, VIDIOC_REQBUFS, unsafe.Pointer(&rb))
}

// enqueue buffer to device
func enqueue(fd int, index int) error {
	// fmt.Println("enqueue buffer to device")
	qbuf := v4l2_buffer{
		typ:    V4L2_BUF_TYPE_VIDEO_CAPTURE,
		memory: V4L2_MEMORY_MMAP,
		index:  uint32(index),
	}
	return ioctl(fd, VIDIOC_QBUF, unsafe.Pointer(&qbuf))
}

// dequeue next buffer from device
func dequeue(fd int) (int, int, error) {
	// fmt.Println("dequeue buffer to device")

	dqbuf := v4l2_buffer{
		typ: V4L2_BUF_TYPE_VIDEO_CAPTURE,
	}
	err := ioctl(fd, VIDIOC_DQBUF, unsafe.Pointer(&dqbuf))
	return int(dqbuf.index), int(dqbuf.bytesused), err
}
