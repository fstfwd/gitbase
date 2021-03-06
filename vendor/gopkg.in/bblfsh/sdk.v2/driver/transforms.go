package driver

import (
	"context"

	"github.com/opentracing/opentracing-go"

	"gopkg.in/bblfsh/sdk.v2/uast/nodes"
	"gopkg.in/bblfsh/sdk.v2/uast/transformer"
)

// Transforms describes a set of AST transformations this driver requires.
type Transforms struct {
	// Namespace for this language driver
	Namespace string
	// Preprocess transforms normalizes native AST.
	// It usually includes:
	//	* changing type key to uast.KeyType
	//	* changing token key to uast.KeyToken
	//	* restructure positional information
	Preprocess []transformer.Transformer
	// Normalize converts known AST structures to high-level AST representation (UAST).
	Normalize []transformer.Transformer
	// Annotations transforms annotates the tree with roles.
	Annotations []transformer.Transformer
	// Code transforms are applied directly after Native and provide a way
	// to extract more information from source files, fix positional info, etc.
	Code []transformer.CodeTransformer
}

// Do applies AST transformation pipeline for specified nodes.
func (t Transforms) Do(rctx context.Context, mode Mode, code string, nd nodes.Node) (nodes.Node, error) {
	sp, ctx := opentracing.StartSpanFromContext(rctx, "uast.Transform")
	defer sp.Finish()

	if mode > ModeSemantic {
		return nil, ErrModeNotSupported.New()
	}
	if mode == 0 {
		mode = ModeDefault
	}
	if mode == ModeNative {
		return nd, nil
	}

	var err error

	nd, err = t.do(ctx, mode, nd)
	if err != nil {
		return nd, err
	}

	nd, err = t.doCode(ctx, code, nd)
	if err != nil {
		return nd, err
	}

	nd, err = t.doPost(ctx, mode, nd)
	if err != nil {
		return nd, err
	}

	return nd, nil
}

func (t Transforms) do(ctx context.Context, mode Mode, nd nodes.Node) (nodes.Node, error) {
	var err error
	runAll := func(name string, list []transformer.Transformer) error {
		sp, _ := opentracing.StartSpanFromContext(ctx, "uast.Transform."+name)
		defer sp.Finish()

		for _, t := range list {
			nd, err = t.Do(nd)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := runAll("preprocess", t.Preprocess); err != nil {
		return nd, err
	}
	if mode >= ModeSemantic {
		if err := runAll("semantic", t.Normalize); err != nil {
			return nd, err
		}
	}
	if mode >= ModeAnnotated {
		if err := runAll("annotated", t.Annotations); err != nil {
			return nd, err
		}
	}
	return nd, nil
}

func (t Transforms) doCode(ctx context.Context, code string, nd nodes.Node) (nodes.Node, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "uast.Transform.onCode")
	defer sp.Finish()

	var err error
	for _, ct := range t.Code {
		t := ct.OnCode(code)
		nd, err = t.Do(nd)
		if err != nil {
			return nd, err
		}
	}
	return nd, nil
}

func (t Transforms) doPost(ctx context.Context, mode Mode, nd nodes.Node) (nodes.Node, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "uast.Transform.namespace")
	defer sp.Finish()

	var err error
	if mode >= ModeSemantic && t.Namespace != "" {
		nd, err = transformer.DefaultNamespace(t.Namespace).Do(nd)
		if err != nil {
			return nd, err
		}
	}
	return nd, nil
}
