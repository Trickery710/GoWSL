package gowsl_test

import (
	wsl "github.com/ubuntu/gowsl"

	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShell(t *testing.T) {
	realDistro := newTestDistro(t, rootFs)
	fakeDistro := wsl.NewDistro(uniqueDistroName(t))
	wrongDistro := wsl.NewDistro("I have a \x00 null char in my name")

	cmdExit0 := "exit 0"
	cmdExit42 := "exit 42"

	cmdCheckNotCWD := "[ `pwd` = /root ]"
	cmdCheckCWD := "[ `pwd` != /root ]"

	wrongCommand := "echo 'Oh no!, There is a \x00 in my command!'"

	testCases := map[string]struct {
		withCwd      bool
		withCommand  *string
		distro       *wsl.Distro
		wantError    bool
		wantExitCode uint32
	}{
		// Test with no arguments
		"happy path":   {distro: &realDistro},
		"fake distro":  {distro: &fakeDistro, wantError: true},
		"wrong distro": {distro: &wrongDistro, wantError: true},

		// Test UseCWD
		"success with CWD": {distro: &realDistro, withCwd: true},
		"failure with CWD": {distro: &fakeDistro, withCwd: true, wantError: true},

		// Test withCommand
		"success with command":              {distro: &realDistro, withCommand: &cmdExit0},
		"failure command with exit error":   {distro: &realDistro, withCommand: &cmdExit42, wantError: true, wantExitCode: 42},
		"failure with null char in command": {distro: &realDistro, withCommand: &wrongCommand, wantError: true},

		// Test that UseCWD actually changes the working directory
		"ensure default is not CWD": {distro: &realDistro, withCommand: &cmdCheckNotCWD},
		"ensure UseCWD uses CWD":    {distro: &realDistro, withCwd: true, withCommand: &cmdCheckCWD},
	}

	for name, tc := range testCases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			d := *tc.distro

			// Keeping distro awake so there are no unexpected timeouts
			if d == realDistro {
				defer keepAwake(t, context.Background(), &realDistro)()
			}

			// Because Shell is an interactive command, it needs to be quit from
			// outside. This goroutine sets a fuse before shutting down the distro.
			// Some commands can escape on their own. Using `done` skips the
			// termination, preventing unsuccessful exit codes.
			timeout := make(chan time.Duration)
			done := make(chan struct{})
			go func() {
				time.Sleep(<-timeout)
				select {
				case <-done:
				default:
					t.Logf("Command timed out")
					err := d.Terminate()
					if err != nil {
						t.Log(err)
					}
					<-done
				}
			}()

			var err error
			if tc.withCwd && tc.withCommand != nil {
				timeout <- 3 * time.Second
				err = d.Shell(wsl.WithCommand(*tc.withCommand), wsl.UseCWD())
				done <- struct{}{}
			} else if tc.withCwd {
				timeout <- 3 * time.Second
				err = d.Shell(wsl.UseCWD())
				done <- struct{}{}
			} else if tc.withCommand != nil {
				timeout <- 3 * time.Second
				err = d.Shell(wsl.WithCommand(*tc.withCommand))
				done <- struct{}{}
			} else {
				timeout <- 3 * time.Second
				err = d.Shell()
				done <- struct{}{}
			}
			close(timeout)
			close(done)

			if !tc.wantError {
				require.NoError(t, err, "Unexpected error after Distro.Shell")
				return
			}

			require.Error(t, err, "Unexpected success after Distro.Shell")

			var target *wsl.ShellError
			if tc.wantExitCode == 0 {
				notErrorAsf(t, err, &target, "unexpected ShellError, expected any other type")
				return
			}

			require.ErrorAs(t, err, &target, "unexpected error type, expected a ShellError")
			require.Equal(t, tc.wantExitCode, target.ExitCode(), "Unexpected value for ExitCode returned from Distro.Shell")
		})
	}
}
