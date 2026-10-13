package main

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	frameMagic  = 0x57525043
	headerSize  = 24
	maxRPCBytes = 16 << 20

	pluginRequest  = 1
	pluginResponse = 2
	hostRequest    = 3
	hostResponse   = 4
	shutdown       = 5
)

var errProtocol = errors.New("Invalid plugin IPC frame.")

type frame struct {
	kind    uint32
	id      uint64
	status  uint32
	payload []byte
}

func readFrame(reader io.Reader) (frame, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return frame{}, err
	}
	f := frame{
		kind:   binary.LittleEndian.Uint32(header[4:8]),
		id:     binary.LittleEndian.Uint64(header[8:16]),
		status: binary.LittleEndian.Uint32(header[16:20]),
	}
	length := binary.LittleEndian.Uint32(header[20:24])
	if binary.LittleEndian.Uint32(header[:4]) != frameMagic || f.id == 0 || length > maxRPCBytes {
		return frame{}, errProtocol
	}
	if f.kind < pluginRequest || f.kind > shutdown {
		return frame{}, errProtocol
	}
	request := f.kind == pluginRequest || f.kind == hostRequest || f.kind == shutdown
	if (request && f.status != 0) || (f.kind == shutdown && length != 0) {
		return frame{}, errProtocol
	}
	f.payload = make([]byte, int(length))
	if _, err := io.ReadFull(reader, f.payload); err != nil {
		return frame{}, err
	}
	return f, nil
}

func writeFrame(writer io.Writer, f frame) error {
	if len(f.payload) > maxRPCBytes || f.id == 0 {
		return errProtocol
	}
	var header [headerSize]byte
	binary.LittleEndian.PutUint32(header[:4], frameMagic)
	binary.LittleEndian.PutUint32(header[4:8], f.kind)
	binary.LittleEndian.PutUint64(header[8:16], f.id)
	binary.LittleEndian.PutUint32(header[16:20], f.status)
	binary.LittleEndian.PutUint32(header[20:24], uint32(len(f.payload)))
	if err := writeAll(writer, header[:]); err != nil {
		return err
	}
	return writeAll(writer, f.payload)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
