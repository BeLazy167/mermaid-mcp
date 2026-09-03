//go:build !linux

package main

import "errors"

func restrictNetwork() error {
	return errors.New("renderer network isolation requires Linux")
}
