package tabula

import (
	"fmt"
	"slices"
	"strconv"
)

type writeBatch struct {
	sizes   []int
	aliases map[int][]map[*typedInput]*typedInput
}

type writeChunk struct {
	shape  *compiledShape
	values []Binding
}

func writeForms(node writeDeclaration) (map[string]statementNode, *writeBatch, error) {
	if len(node.batch) == 0 {
		return map[string]statementNode{"write": node}, nil, nil
	}
	if node.command != "INSERT" || len(node.rows) != 1 || node.defaults || node.selection != nil || len(node.tables) != 0 {
		return nil, nil, fmt.Errorf("tabula: Batch requires one VALUES template without modifying common tables")
	}
	batch := &writeBatch{sizes: slices.Clone(node.batch), aliases: make(map[int][]map[*typedInput]*typedInput)}
	slices.Sort(batch.sizes)
	if batch.sizes[0] != 1 {
		return nil, nil, fmt.Errorf("tabula: Batch must include size 1 for remainders")
	}
	forms := make(map[string]statementNode)
	for _, size := range batch.sizes {
		if size < 1 || size > 65535 || batch.aliases[size] != nil {
			return nil, nil, fmt.Errorf("tabula: invalid or duplicate Batch row count")
		}
		n := node
		n.rows = make([][]RowAssignment, size)
		n.aliases = make([]map[*typedInput]*typedInput, size)
		for i := range size {
			n.rows[i] = node.rows[0]
			n.aliases[i] = make(map[*typedInput]*typedInput)
		}
		batch.aliases[size] = n.aliases
		forms[batchForm(size)] = n
	}
	slices.Reverse(batch.sizes)
	return forms, batch, nil
}

func batchForm(size int) string { return "rows." + strconv.Itoa(size) }

func (batch *writeBatch) prepare(family *rowFamily, unscoped bool, rows [][]Binding, common []Binding) ([]writeChunk, error) {
	var chunks []writeChunk
	for offset := 0; offset < len(rows); {
		size := 1
		for _, candidate := range batch.sizes {
			if candidate <= len(rows)-offset {
				size = candidate
				break
			}
		}
		shape, err := family.variant(unscoped, nil, batchForm(size))
		if err != nil {
			return nil, err
		}
		values := slices.Clone(common)
		for i, row := range rows[offset : offset+size] {
			aliases := batch.aliases[size][i]
			seen := make(map[*typedInput]bool)
			for _, value := range row {
				alias := aliases[value.input]
				if alias == nil || seen[value.input] {
					return nil, fmt.Errorf("tabula: batch row %d has an unknown or duplicate binding", offset+i+1)
				}
				seen[value.input] = true
				if value.err != nil {
					return nil, fmt.Errorf("tabula: encode batch row %d: %w", offset+i+1, value.err)
				}
				value.input = alias
				values = append(values, value)
			}
			if len(seen) != len(aliases) {
				return nil, fmt.Errorf("tabula: batch row %d is missing bindings", offset+i+1)
			}
		}
		chunks = append(chunks, writeChunk{shape, writeBindings(shape, values, family.live)})
		offset += size
	}
	return chunks, nil
}
