package main

import (
	"os/user"
	goruntime "runtime"
)

func platformKey() string { return goruntime.GOOS }

func userCurrentName() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
