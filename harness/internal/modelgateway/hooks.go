package modelgateway

import "context"

type ModelHooks struct {
	BeforeModel []BeforeModelHook
	AfterModel  []AfterModelHook
}

type BeforeModelHook func(ctx context.Context, req ModelRequest, target ModelTarget) (ModelRequest, error)
type AfterModelHook func(ctx context.Context, req ModelRequest, target ModelTarget, resp ModelResponse) (ModelResponse, error)

func (h ModelHooks) RunBefore(ctx context.Context, req ModelRequest, target ModelTarget) (ModelRequest, error) {
	var err error
	for _, hook := range h.BeforeModel {
		if hook == nil {
			continue
		}
		req, err = hook(ctx, req, target)
		if err != nil {
			return req, err
		}
	}
	return req, nil
}

func (h ModelHooks) RunAfter(ctx context.Context, req ModelRequest, target ModelTarget, resp ModelResponse) (ModelResponse, error) {
	var err error
	for _, hook := range h.AfterModel {
		if hook == nil {
			continue
		}
		resp, err = hook(ctx, req, target, resp)
		if err != nil {
			return resp, err
		}
	}
	return resp, nil
}
