package model

import (
	_ "embed"
)

// SafeTensors contains the pre-trained weights for the BASE8M model (~28.8 MB).
//
//go:embed model.safetensors
var SafeTensors []byte

// TokenizerJSON contains the pre-trained tokenizer vocabulary and configuration (~683 KB).
//
//go:embed tokenizer.json
var TokenizerJSON []byte
