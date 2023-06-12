// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MIT

// +build !windows

package metrics

import (
	"syscall"
)

const (
	// DefaultSignal is used with DefaultInmemSignal
	DefaultSignal = syscall.SIGUSR1
)
