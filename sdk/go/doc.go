// Package jdix is the Go client for jdix-sandbox.
//
// The shape to aim for is "as easy as os/exec, but somewhere else":
//
//	c, _ := jdix.NewClient(jdix.WithAPIKey(os.Getenv("JDIX_API_KEY")))
//	sbx, err := c.Create(ctx, jdix.CreateOpts{Template: "py312-small", TTL: 10 * time.Minute})
//	defer sbx.Close(ctx)
//
//	res, _ := sbx.Run(ctx, "go version")
//	fmt.Println(res.ExitCode, res.Stdout)
//
// Two conventions are worth knowing up front.
//
// A command that exits non-zero is not a Go error. Run returns a Result whose
// ExitCode says what happened, exactly as os/exec's caller would inspect it;
// errors are reserved for the request failing to happen at all. Conflating the
// two forces every caller to unwrap an error just to read an exit status.
//
// Files are io.Reader and io.Writer, never []byte. Sandboxes routinely produce
// artefacts far larger than anything worth holding in memory.
package jdix
