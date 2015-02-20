package engine

import (
	"fmt"

	"github.com/keybase/go/libkb"
)

type Engine interface {
	Run(ctx *Context, args interface{}, reply interface{}) error
	libkb.UIConsumer
}

func RunEngine(e Engine, ctx *Context, args interface{}, reply interface{}) error {
	if err := check(e, ctx); err != nil {
		return err
	}
	return e.Run(ctx, args, reply)
}

func check(c libkb.UIConsumer, ctx *Context) error {
	if err := checkUI(c, ctx); err != nil {
		return fmt.Errorf("%s: %s", c.Name(), err)
	}

	for _, sub := range c.SubConsumers() {
		if err := check(sub, ctx); err != nil {
			return fmt.Errorf("%s: %s", sub.Name(), err)
		}
	}

	return nil
}

func checkUI(c libkb.UIConsumer, ctx *Context) error {
	for _, ui := range c.RequiredUIs() {
		if !ctx.HasUI(ui) {
			return fmt.Errorf("requires ui %q", ui)
		}
	}
	return nil
}
