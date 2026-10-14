package accumulo

import "context"

// ListTableSplits lists the current bounded split points for table name in
// Accumulo tablet order. Each returned row is copied and safe for caller
// mutation. The final unbounded tablet extent is not reported as a split.
func (c *Connector) ListTableSplits(ctx context.Context, name string) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	table, err := c.TableByName(ctx, name)
	if err != nil {
		return nil, err
	}
	tablets, err := c.Tablets(ctx, table)
	if err != nil {
		return nil, err
	}
	splits := make([][]byte, 0, len(tablets))
	for i := range tablets {
		if tablets[i].Extent.EndRow == nil {
			continue
		}
		splits = append(splits, cloneRow(tablets[i].Extent.EndRow))
	}
	return splits, nil
}
