// Copyright 2019 Path Network, Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"go.uber.org/zap"
	"io"
	"net"
	"strconv"
)

func tcpCopyData(dst net.Conn, src net.Conn, ch chan<- error) {
	_, err := io.Copy(dst, src)
	ch <- err
}

func tcpHandleConnection(conn net.Conn, logger *zap.Logger) {
	defer conn.Close()
	logger = logger.With(zap.String("remoteAddr", conn.RemoteAddr().String()),
		zap.String("localAddr", conn.LocalAddr().String()))

	if !CheckOriginAllowed(conn.RemoteAddr().(*net.TCPAddr).IP) {
		logger.Debug("connection origin not in allowed subnets", zap.Bool("dropConnection", true))
		return
	}

	if Opts.Verbose > 1 {
		logger.Debug("new connection")
	}

	buffer := GetBuffer()
	defer func() {
		if buffer != nil {
			PutBuffer(buffer)
		}
	}()

	n, err := conn.Read(buffer)
	if err != nil {
		logger.Debug("failed to read PROXY header", zap.Error(err), zap.Bool("dropConnection", true))
		return
	}

	saddr, _, restBytes, err := PROXYReadRemoteAddr(buffer[:n], TCP)
	if err != nil {
		logger.Debug("failed to parse PROXY header", zap.Error(err), zap.Bool("dropConnection", true))
		return
	}

	port := []rune("190.115.196.10:10000")[Opts.ListenAddrLen:Opts.ListenAddrLen+5]
	targetAddr := Opts.TargetAddr6 + ":" + string(port)
	if AddrVersion(saddr) == 4 {
		targetAddr = Opts.TargetAddr4 + ":" + string(port)
	}

	clientAddr := "UNKNOWN"
	if saddr != nil {
		clientAddr = saddr.String()
	}
	logger = logger.With(zap.String("clientAddr", clientAddr), zap.String("targetAddr", targetAddr))
	if Opts.Verbose > 1 {
		logger.Debug("successfully parsed PROXY header")
	}

	dialer := net.Dialer{LocalAddr: saddr}
	if saddr != nil {
		dialer.Control = DialUpstreamControl(saddr.(*net.TCPAddr).Port)
	}
	upstreamConn, err := dialer.Dial("tcp", targetAddr)
	if err != nil {
		logger.Debug("failed to establish upstream connection", zap.Error(err), zap.Bool("dropConnection", true))
		return
	}

	defer upstreamConn.Close()
	if Opts.Verbose > 1 {
		logger.Debug("successfully established upstream connection")
	}

	if err := conn.(*net.TCPConn).SetNoDelay(true); err != nil {
		logger.Debug("failed to set nodelay on downstream connection", zap.Error(err), zap.Bool("dropConnection", true))
	} else if Opts.Verbose > 1 {
		logger.Debug("successfully set NoDelay on downstream connection")
	}

	if err := upstreamConn.(*net.TCPConn).SetNoDelay(true); err != nil {
		logger.Debug("failed to set nodelay on upstream connection", zap.Error(err), zap.Bool("dropConnection", true))
	} else if Opts.Verbose > 1 {
		logger.Debug("successfully set NoDelay on upstream connection")
	}

	for len(restBytes) > 0 {
		n, err := upstreamConn.Write(restBytes)
		if err != nil {
			logger.Debug("failed to write data to upstream connection",
				zap.Error(err), zap.Bool("dropConnection", true))
			return
		}
		restBytes = restBytes[n:]
	}

	PutBuffer(buffer)
	buffer = nil

	outErr := make(chan error, 2)
	go tcpCopyData(upstreamConn, conn, outErr)
	go tcpCopyData(conn, upstreamConn, outErr)

	err = <-outErr
	if err != nil {
		logger.Debug("connection broken", zap.Error(err), zap.Bool("dropConnection", true))
	} else if Opts.Verbose > 1 {
		logger.Debug("connection closing")
	}
}

func TCPListen(listenConfig *net.ListenConfig, logger *zap.Logger, errors chan<- error) {
	ctx := context.Background()

	conns := make(chan net.Conn)
	errs := make(chan error)

	for port := Opts.StartPort; port < Opts.EndPort; port++ {
		go func() {
			ln, err := listenConfig.Listen(ctx, "tcp", Opts.ListenAddr + ":" + strconv.Itoa(port))
			if err != nil {
				logger.Error("failed to bind listener", zap.Error(err))
				errors <- err
				return
			}

			for {
				conn, err := ln.Accept()
				conns <- conn
				errs <- err
			}
		}()
	}

	logger.Info("listening")

	go func() {
		for {
			err := <- errs
			if err != nil {
				logger.Error("failed to accept new connection", zap.Error(err))
				errors <- err
				return
			}
		}
	}()

	for {
		conn := <- conns

		go tcpHandleConnection(conn, logger)
	}
}
