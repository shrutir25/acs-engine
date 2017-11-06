package query

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxql"
)

// CompileOptions are the customization options for the compiler.
type CompileOptions struct {
	Now time.Time
}

// Statement is a compiled query statement.
type Statement interface {
	// Prepare prepares the statement by mapping shards and finishing the creation
	// of the query plan.
	Prepare(shardMapper ShardMapper, opt SelectOptions) (PreparedStatement, error)
}

// compiledStatement represents a select statement that has undergone some initial processing to
// determine if it is valid and to have some initial modifications done on the AST.
type compiledStatement struct {
	// Condition is the condition used for accessing data.
	Condition influxql.Expr

	// TimeRange is the TimeRange for selecting data.
	TimeRange influxql.TimeRange

	// Interval holds the time grouping interval.
	Interval Interval

	// InheritedInterval marks if the interval was inherited by a parent.
	// If this is set, then an interval that was inherited will not cause
	// a query that shouldn't have an interval to fail.
	InheritedInterval bool

	// Ascending is true if the time ordering is ascending.
	Ascending bool

	// FunctionCalls holds a reference to the call expression of every function
	// call that has been encountered.
	FunctionCalls []*influxql.Call

	// OnlySelectors is set to true when there are no aggregate functions.
	OnlySelectors bool

	// HasDistinct is set when the distinct() function is encountered.
	HasDistinct bool

	// FillOption contains the fill option for aggregates.
	FillOption influxql.FillOption

	// TopBottomFunction is set to top or bottom when one of those functions are
	// used in the statement.
	TopBottomFunction string

	// HasAuxiliaryFields is true when the function requires auxiliary fields.
	HasAuxiliaryFields bool

	// Fields holds all of the fields that will be used.
	Fields []*compiledField

	// TimeFieldName stores the name of the time field's column.
	// The column names generated by the compiler will not conflict with
	// this name.
	TimeFieldName string

	// Limit is the number of rows per series this query should be limited to.
	Limit int

	// HasTarget is true if this query is being written into a target.
	HasTarget bool

	// Options holds the configured compiler options.
	Options CompileOptions

	stmt *influxql.SelectStatement
}

func newCompiler(opt CompileOptions) *compiledStatement {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	return &compiledStatement{
		OnlySelectors: true,
		TimeFieldName: "time",
		Options:       opt,
	}
}

func Compile(stmt *influxql.SelectStatement, opt CompileOptions) (Statement, error) {
	c := newCompiler(opt)
	if err := c.preprocess(stmt); err != nil {
		return nil, err
	}
	if err := c.compile(stmt); err != nil {
		return nil, err
	}
	c.stmt = stmt.Clone()
	c.stmt.TimeAlias = c.TimeFieldName
	c.stmt.Condition = c.Condition

	// Convert DISTINCT into a call.
	c.stmt.RewriteDistinct()

	// Remove "time" from fields list.
	c.stmt.RewriteTimeFields()

	// Rewrite any regex conditions that could make use of the index.
	c.stmt.RewriteRegexConditions()
	return c, nil
}

// preprocess retrieves and records the global attributes of the current statement.
func (c *compiledStatement) preprocess(stmt *influxql.SelectStatement) error {
	c.Ascending = stmt.TimeAscending()
	c.Limit = stmt.Limit
	c.HasTarget = stmt.Target != nil

	valuer := influxql.NowValuer{Now: c.Options.Now, Location: stmt.Location}
	if cond, t, err := influxql.ConditionExpr(stmt.Condition, &valuer); err != nil {
		return err
	} else {
		c.Condition = cond
		c.TimeRange = t
	}

	// Read the dimensions of the query, validate them, and retrieve the interval
	// if it exists.
	if err := c.compileDimensions(stmt); err != nil {
		return err
	}

	// Retrieve the fill option for the statement.
	c.FillOption = stmt.Fill

	// Resolve the min and max times now that we know if there is an interval or not.
	if c.TimeRange.Min.IsZero() {
		c.TimeRange.Min = time.Unix(0, influxql.MinTime).UTC()
	}
	if c.TimeRange.Max.IsZero() {
		// If the interval is non-zero, then we have an aggregate query and
		// need to limit the maximum time to now() for backwards compatibility
		// and usability.
		if !c.Interval.IsZero() {
			c.TimeRange.Max = c.Options.Now
		} else {
			c.TimeRange.Max = time.Unix(0, influxql.MaxTime).UTC()
		}
	}
	return nil
}

func (c *compiledStatement) compile(stmt *influxql.SelectStatement) error {
	if err := c.compileFields(stmt); err != nil {
		return err
	}
	if err := c.validateFields(); err != nil {
		return err
	}

	// Look through the sources and compile each of the subqueries (if they exist).
	// We do this after compiling the outside because subqueries may require
	// inherited state.
	for _, source := range stmt.Sources {
		switch source := source.(type) {
		case *influxql.SubQuery:
			if err := c.subquery(source.Statement); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *compiledStatement) compileFields(stmt *influxql.SelectStatement) error {
	c.Fields = make([]*compiledField, 0, len(stmt.Fields))
	for _, f := range stmt.Fields {
		// Remove any time selection (it is automatically selected by default)
		// and set the time column name to the alias of the time field if it exists.
		// Such as SELECT time, max(value) FROM cpu will be SELECT max(value) FROM cpu
		// and SELECT time AS timestamp, max(value) FROM cpu will return "timestamp"
		// as the column name for the time.
		if ref, ok := f.Expr.(*influxql.VarRef); ok && ref.Val == "time" {
			if f.Alias != "" {
				c.TimeFieldName = f.Alias
			}
			continue
		}

		// Append this field to the list of processed fields and compile it.
		field := &compiledField{
			global:        c,
			Field:         f,
			AllowWildcard: true,
		}
		c.Fields = append(c.Fields, field)
		if err := field.compileExpr(f.Expr); err != nil {
			return err
		}
	}
	return nil
}

type compiledField struct {
	// This holds the global state from the compiled statement.
	global *compiledStatement

	// Field is the top level field that is being compiled.
	Field *influxql.Field

	// AllowWildcard is set to true if a wildcard or regular expression is allowed.
	AllowWildcard bool
}

// compileExpr creates the node that executes the expression and connects that
// node to the WriteEdge as the output.
func (c *compiledField) compileExpr(expr influxql.Expr) error {
	switch expr := expr.(type) {
	case *influxql.VarRef:
		// A bare variable reference will require auxiliary fields.
		c.global.HasAuxiliaryFields = true
		return nil
	case *influxql.Wildcard:
		// Wildcards use auxiliary fields. We assume there will be at least one
		// expansion.
		c.global.HasAuxiliaryFields = true
		if !c.AllowWildcard {
			return errors.New("unable to use wildcard in a binary expression")
		}
		return nil
	case *influxql.RegexLiteral:
		if !c.AllowWildcard {
			return errors.New("unable to use regex in a binary expression")
		}
		c.global.HasAuxiliaryFields = true
		return nil
	case *influxql.Call:
		// Register the function call in the list of function calls.
		c.global.FunctionCalls = append(c.global.FunctionCalls, expr)

		switch expr.Name {
		case "percentile":
			return c.compilePercentile(expr.Args)
		case "sample":
			return c.compileSample(expr.Args)
		case "distinct":
			return c.compileDistinct(expr.Args)
		case "top", "bottom":
			return c.compileTopBottom(expr)
		case "derivative", "non_negative_derivative":
			isNonNegative := expr.Name == "non_negative_derivative"
			return c.compileDerivative(expr.Args, isNonNegative)
		case "difference", "non_negative_difference":
			isNonNegative := expr.Name == "non_negative_difference"
			return c.compileDifference(expr.Args, isNonNegative)
		case "cumulative_sum":
			return c.compileCumulativeSum(expr.Args)
		case "moving_average":
			return c.compileMovingAverage(expr.Args)
		case "elapsed":
			return c.compileElapsed(expr.Args)
		case "integral":
			return c.compileIntegral(expr.Args)
		case "holt_winters", "holt_winters_with_fit":
			withFit := expr.Name == "holt_winters_with_fit"
			return c.compileHoltWinters(expr.Args, withFit)
		default:
			return c.compileFunction(expr)
		}
	case *influxql.Distinct:
		call := expr.NewCall()
		c.global.FunctionCalls = append(c.global.FunctionCalls, call)
		return c.compileDistinct(call.Args)
	case *influxql.BinaryExpr:
		// Disallow wildcards in binary expressions. RewriteFields, which expands
		// wildcards, is too complicated if we allow wildcards inside of expressions.
		c.AllowWildcard = false

		// Check if either side is a literal so we only compile one side if it is.
		if _, ok := expr.LHS.(influxql.Literal); ok {
			if _, ok := expr.RHS.(influxql.Literal); ok {
				return errors.New("cannot perform a binary expression on two literals")
			}
			return c.compileExpr(expr.RHS)
		} else if _, ok := expr.RHS.(influxql.Literal); ok {
			return c.compileExpr(expr.LHS)
		} else {
			// Validate both sides of the expression.
			if err := c.compileExpr(expr.LHS); err != nil {
				return err
			}
			if err := c.compileExpr(expr.RHS); err != nil {
				return err
			}
			return nil
		}
	case *influxql.ParenExpr:
		return c.compileExpr(expr.Expr)
	}
	return errors.New("unimplemented")
}

func (c *compiledField) compileSymbol(name string, field influxql.Expr) error {
	// Must be a variable reference, wildcard, or regexp.
	switch field.(type) {
	case *influxql.VarRef:
		return nil
	case *influxql.Wildcard:
		if !c.AllowWildcard {
			return fmt.Errorf("unsupported expression with wildcard: %s()", name)
		}
		c.global.OnlySelectors = false
		return nil
	case *influxql.RegexLiteral:
		if !c.AllowWildcard {
			return fmt.Errorf("unsupported expression with regex field: %s()", name)
		}
		c.global.OnlySelectors = false
		return nil
	default:
		return fmt.Errorf("expected field argument in %s()", name)
	}
}

func (c *compiledField) compileFunction(expr *influxql.Call) error {
	// Validate the function call and mark down some meta properties
	// related to the function for query validation.
	switch expr.Name {
	case "max", "min", "first", "last":
		// top/bottom are not included here since they are not typical functions.
	case "count", "sum", "mean", "median", "mode", "stddev", "spread":
		// These functions are not considered selectors.
		c.global.OnlySelectors = false
	default:
		return fmt.Errorf("undefined function %s()", expr.Name)
	}

	if exp, got := 1, len(expr.Args); exp != got {
		return fmt.Errorf("invalid number of arguments for %s, expected %d, got %d", expr.Name, exp, got)
	}

	// If this is a call to count(), allow distinct() to be used as the function argument.
	if expr.Name == "count" {
		// If we have count(), the argument may be a distinct() call.
		if arg0, ok := expr.Args[0].(*influxql.Call); ok && arg0.Name == "distinct" {
			return c.compileDistinct(arg0.Args)
		} else if arg0, ok := expr.Args[0].(*influxql.Distinct); ok {
			call := arg0.NewCall()
			return c.compileDistinct(call.Args)
		}
	}
	return c.compileSymbol(expr.Name, expr.Args[0])
}

func (c *compiledField) compilePercentile(args []influxql.Expr) error {
	if exp, got := 2, len(args); got != exp {
		return fmt.Errorf("invalid number of arguments for percentile, expected %d, got %d", exp, got)
	}

	switch args[1].(type) {
	case *influxql.IntegerLiteral:
	case *influxql.NumberLiteral:
	default:
		return fmt.Errorf("expected float argument in percentile()")
	}
	return c.compileSymbol("percentile", args[0])
}

func (c *compiledField) compileSample(args []influxql.Expr) error {
	if exp, got := 2, len(args); got != exp {
		return fmt.Errorf("invalid number of arguments for sample, expected %d, got %d", exp, got)
	}

	switch arg1 := args[1].(type) {
	case *influxql.IntegerLiteral:
		if arg1.Val <= 0 {
			return fmt.Errorf("sample window must be greater than 1, got %d", arg1.Val)
		}
	default:
		return fmt.Errorf("expected integer argument in sample()")
	}
	return c.compileSymbol("sample", args[0])
}

func (c *compiledField) compileDerivative(args []influxql.Expr, isNonNegative bool) error {
	name := "derivative"
	if isNonNegative {
		name = "non_negative_derivative"
	}

	if min, max, got := 1, 2, len(args); got > max || got < min {
		return fmt.Errorf("invalid number of arguments for %s, expected at least %d but no more than %d, got %d", name, min, max, got)
	}

	// Retrieve the duration from the derivative() call, if specified.
	if len(args) == 2 {
		switch arg1 := args[1].(type) {
		case *influxql.DurationLiteral:
			if arg1.Val <= 0 {
				return fmt.Errorf("duration argument must be positive, got %s", influxql.FormatDuration(arg1.Val))
			}
		default:
			return fmt.Errorf("second argument to %s must be a duration, got %T", name, args[1])
		}
	}
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("%s aggregate requires a GROUP BY interval", name)
		}
		return c.compileExpr(arg0)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to %s", name)
		}
		return c.compileSymbol(name, arg0)
	}
}

func (c *compiledField) compileElapsed(args []influxql.Expr) error {
	if min, max, got := 1, 2, len(args); got > max || got < min {
		return fmt.Errorf("invalid number of arguments for elapsed, expected at least %d but no more than %d, got %d", min, max, got)
	}

	// Retrieve the duration from the elapsed() call, if specified.
	if len(args) == 2 {
		switch arg1 := args[1].(type) {
		case *influxql.DurationLiteral:
			if arg1.Val <= 0 {
				return fmt.Errorf("duration argument must be positive, got %s", influxql.FormatDuration(arg1.Val))
			}
		default:
			return fmt.Errorf("second argument to elapsed must be a duration, got %T", args[1])
		}
	}
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("elapsed aggregate requires a GROUP BY interval")
		}
		return c.compileExpr(arg0)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to elapsed")
		}
		return c.compileSymbol("elapsed", arg0)
	}
}

func (c *compiledField) compileDifference(args []influxql.Expr, isNonNegative bool) error {
	name := "difference"
	if isNonNegative {
		name = "non_negative_difference"
	}

	if got := len(args); got != 1 {
		return fmt.Errorf("invalid number of arguments for %s, expected 1, got %d", name, got)
	}
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("%s aggregate requires a GROUP BY interval", name)
		}
		return c.compileExpr(arg0)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to %s", name)
		}
		return c.compileSymbol(name, arg0)
	}
}

func (c *compiledField) compileCumulativeSum(args []influxql.Expr) error {
	if got := len(args); got != 1 {
		return fmt.Errorf("invalid number of arguments for cumulative_sum, expected 1, got %d", got)
	}
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("cumulative_sum aggregate requires a GROUP BY interval")
		}
		return c.compileExpr(arg0)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to cumulative_sum")
		}
		return c.compileSymbol("cumulative_sum", arg0)
	}
}

func (c *compiledField) compileMovingAverage(args []influxql.Expr) error {
	if got := len(args); got != 2 {
		return fmt.Errorf("invalid number of arguments for moving_average, expected 2, got %d", got)
	}

	switch arg1 := args[1].(type) {
	case *influxql.IntegerLiteral:
		if arg1.Val <= 1 {
			return fmt.Errorf("moving_average window must be greater than 1, got %d", arg1.Val)
		}
	default:
		return fmt.Errorf("second argument for moving_average must be an integer, got %T", args[1])
	}
	c.global.OnlySelectors = false

	// Must be a variable reference, function, wildcard, or regexp.
	switch arg0 := args[0].(type) {
	case *influxql.Call:
		if c.global.Interval.IsZero() {
			return fmt.Errorf("moving_average aggregate requires a GROUP BY interval")
		}
		return c.compileExpr(arg0)
	default:
		if !c.global.Interval.IsZero() {
			return fmt.Errorf("aggregate function required inside the call to moving_average")
		}
		return c.compileSymbol("moving_average", arg0)
	}
}

func (c *compiledField) compileIntegral(args []influxql.Expr) error {
	if min, max, got := 1, 2, len(args); got > max || got < min {
		return fmt.Errorf("invalid number of arguments for integral, expected at least %d but no more than %d, got %d", min, max, got)
	}

	if len(args) == 2 {
		switch arg1 := args[1].(type) {
		case *influxql.DurationLiteral:
			if arg1.Val <= 0 {
				return fmt.Errorf("duration argument must be positive, got %s", influxql.FormatDuration(arg1.Val))
			}
		default:
			return errors.New("second argument must be a duration")
		}
	}
	c.global.OnlySelectors = false

	// Must be a variable reference, wildcard, or regexp.
	return c.compileSymbol("integral", args[0])
}

func (c *compiledField) compileHoltWinters(args []influxql.Expr, withFit bool) error {
	name := "holt_winters"
	if withFit {
		name = "holt_winters_with_fit"
	}

	if exp, got := 3, len(args); got != exp {
		return fmt.Errorf("invalid number of arguments for %s, expected %d, got %d", name, exp, got)
	}

	n, ok := args[1].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer argument as second arg in %s", name)
	} else if n.Val <= 0 {
		return fmt.Errorf("second arg to %s must be greater than 0, got %d", name, n.Val)
	}

	s, ok := args[2].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer argument as third arg in %s", name)
	} else if s.Val < 0 {
		return fmt.Errorf("third arg to %s cannot be negative, got %d", name, s.Val)
	}
	c.global.OnlySelectors = false

	call, ok := args[0].(*influxql.Call)
	if !ok {
		return fmt.Errorf("must use aggregate function with %s", name)
	} else if c.global.Interval.IsZero() {
		return fmt.Errorf("%s aggregate requires a GROUP BY interval", name)
	}
	return c.compileExpr(call)
}

func (c *compiledField) compileDistinct(args []influxql.Expr) error {
	if len(args) == 0 {
		return errors.New("distinct function requires at least one argument")
	} else if len(args) != 1 {
		return errors.New("distinct function can only have one argument")
	}

	if _, ok := args[0].(*influxql.VarRef); !ok {
		return errors.New("expected field argument in distinct()")
	}
	c.global.HasDistinct = true
	c.global.OnlySelectors = false
	return nil
}

func (c *compiledField) compileTopBottom(call *influxql.Call) error {
	if c.global.TopBottomFunction != "" {
		return fmt.Errorf("selector function %s() cannot be combined with other functions", c.global.TopBottomFunction)
	}

	if exp, got := 2, len(call.Args); got < exp {
		return fmt.Errorf("invalid number of arguments for %s, expected at least %d, got %d", call.Name, exp, got)
	}

	limit, ok := call.Args[len(call.Args)-1].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer as last argument in %s(), found %s", call.Name, call.Args[len(call.Args)-1])
	} else if limit.Val <= 0 {
		return fmt.Errorf("limit (%d) in %s function must be at least 1", limit.Val, call.Name)
	} else if c.global.Limit > 0 && int(limit.Val) > c.global.Limit {
		return fmt.Errorf("limit (%d) in %s function can not be larger than the LIMIT (%d) in the select statement", limit.Val, call.Name, c.global.Limit)
	}

	if _, ok := call.Args[0].(*influxql.VarRef); !ok {
		return fmt.Errorf("expected first argument to be a field in %s(), found %s", call.Name, call.Args[0])
	}

	if len(call.Args) > 2 {
		for _, v := range call.Args[1 : len(call.Args)-1] {
			ref, ok := v.(*influxql.VarRef)
			if !ok {
				return fmt.Errorf("only fields or tags are allowed in %s(), found %s", call.Name, v)
			}

			// Add a field for each of the listed dimensions when not writing the results.
			if !c.global.HasTarget {
				field := &compiledField{
					global: c.global,
					Field:  &influxql.Field{Expr: ref},
				}
				c.global.Fields = append(c.global.Fields, field)
				if err := field.compileExpr(ref); err != nil {
					return err
				}
			}
		}
	}
	c.global.TopBottomFunction = call.Name
	return nil
}

func (c *compiledStatement) compileDimensions(stmt *influxql.SelectStatement) error {
	for _, d := range stmt.Dimensions {
		switch expr := d.Expr.(type) {
		case *influxql.VarRef:
			if strings.ToLower(expr.Val) == "time" {
				return errors.New("time() is a function and expects at least one argument")
			}
		case *influxql.Call:
			// Ensure the call is time() and it has one or two duration arguments.
			// If we already have a duration
			if expr.Name != "time" {
				return errors.New("only time() calls allowed in dimensions")
			} else if got := len(expr.Args); got < 1 || got > 2 {
				return errors.New("time dimension expected 1 or 2 arguments")
			} else if lit, ok := expr.Args[0].(*influxql.DurationLiteral); !ok {
				return errors.New("time dimension must have duration argument")
			} else if c.Interval.Duration != 0 {
				return errors.New("multiple time dimensions not allowed")
			} else {
				c.Interval.Duration = lit.Val
				if len(expr.Args) == 2 {
					switch lit := expr.Args[1].(type) {
					case *influxql.DurationLiteral:
						c.Interval.Offset = lit.Val % c.Interval.Duration
					case *influxql.TimeLiteral:
						c.Interval.Offset = lit.Val.Sub(lit.Val.Truncate(c.Interval.Duration))
					case *influxql.Call:
						if lit.Name != "now" {
							return errors.New("time dimension offset function must be now()")
						} else if len(lit.Args) != 0 {
							return errors.New("time dimension offset now() function requires no arguments")
						}
						now := c.Options.Now
						c.Interval.Offset = now.Sub(now.Truncate(c.Interval.Duration))
					case *influxql.StringLiteral:
						// If literal looks like a date time then parse it as a time literal.
						if lit.IsTimeLiteral() {
							t, err := lit.ToTimeLiteral(stmt.Location)
							if err != nil {
								return err
							}
							c.Interval.Offset = t.Val.Sub(t.Val.Truncate(c.Interval.Duration))
						} else {
							return errors.New("time dimension offset must be duration or now()")
						}
					default:
						return errors.New("time dimension offset must be duration or now()")
					}
				}
			}
		case *influxql.Wildcard:
		case *influxql.RegexLiteral:
		default:
			return errors.New("only time and tag dimensions allowed")
		}
	}
	return nil
}

// validateFields validates that the fields are mutually compatible with each other.
// This runs at the end of compilation but before linking.
func (c *compiledStatement) validateFields() error {
	// Validate that at least one field has been selected.
	if len(c.Fields) == 0 {
		return errors.New("at least 1 non-time field must be queried")
	}
	// Ensure there are not multiple calls if top/bottom is present.
	if len(c.FunctionCalls) > 1 && c.TopBottomFunction != "" {
		return fmt.Errorf("selector function %s() cannot be combined with other functions", c.TopBottomFunction)
	} else if len(c.FunctionCalls) == 0 {
		switch c.FillOption {
		case influxql.NoFill:
			return errors.New("fill(none) must be used with a function")
		case influxql.LinearFill:
			return errors.New("fill(linear) must be used with a function")
		}
		if !c.Interval.IsZero() && !c.InheritedInterval {
			return errors.New("GROUP BY requires at least one aggregate function")
		}
	}
	// If a distinct() call is present, ensure there is exactly one function.
	if c.HasDistinct && (len(c.FunctionCalls) != 1 || c.HasAuxiliaryFields) {
		return errors.New("aggregate function distinct() cannot be combined with other functions or fields")
	}
	// Validate we are using a selector or raw query if auxiliary fields are required.
	if c.HasAuxiliaryFields {
		if !c.OnlySelectors {
			return fmt.Errorf("mixing aggregate and non-aggregate queries is not supported")
		} else if len(c.FunctionCalls) > 1 {
			return fmt.Errorf("mixing multiple selector functions with tags or fields is not supported")
		}
	}
	return nil
}

// subquery compiles and validates a compiled statement for the subquery using
// this compiledStatement as the parent.
func (c *compiledStatement) subquery(stmt *influxql.SelectStatement) error {
	subquery := newCompiler(c.Options)
	if err := subquery.preprocess(stmt); err != nil {
		return err
	}

	// Substitute now() into the subquery condition. Then use ConditionExpr to
	// validate the expression. Do not store the results. We have no way to store
	// and read those results at the moment.
	valuer := influxql.NowValuer{Now: c.Options.Now, Location: stmt.Location}
	stmt.Condition = influxql.Reduce(stmt.Condition, &valuer)

	// If the ordering is different and the sort field was specified for the subquery,
	// throw an error.
	if len(stmt.SortFields) != 0 && subquery.Ascending != c.Ascending {
		return errors.New("subqueries must be ordered in the same direction as the query itself")
	}
	subquery.Ascending = c.Ascending

	// Find the intersection between this time range and the parent.
	// If the subquery doesn't have a time range, this causes it to
	// inherit the parent's time range.
	subquery.TimeRange = subquery.TimeRange.Intersect(c.TimeRange)

	// If the fill option is null, set it to none so we don't waste time on
	// null values with a redundant fill iterator.
	if !subquery.Interval.IsZero() && subquery.FillOption == influxql.NullFill {
		subquery.FillOption = influxql.NoFill
	}

	// Inherit the grouping interval if the subquery has none.
	if !c.Interval.IsZero() && subquery.Interval.IsZero() {
		subquery.Interval = c.Interval
		subquery.InheritedInterval = true
	}
	return subquery.compile(stmt)
}

func (c *compiledStatement) Prepare(shardMapper ShardMapper, sopt SelectOptions) (PreparedStatement, error) {
	// If this is a query with a grouping, there is a bucket limit, and the minimum time has not been specified,
	// we need to limit the possible time range that can be used when mapping shards but not when actually executing
	// the select statement. Determine the shard time range here.
	timeRange := c.TimeRange
	if sopt.MaxBucketsN > 0 && !c.stmt.IsRawQuery && timeRange.MinTime() == influxql.MinTime {
		interval, err := c.stmt.GroupByInterval()
		if err != nil {
			return nil, err
		}

		offset, err := c.stmt.GroupByOffset()
		if err != nil {
			return nil, err
		}

		if interval > 0 {
			// Determine the last bucket using the end time.
			opt := IteratorOptions{
				Interval: Interval{
					Duration: interval,
					Offset:   offset,
				},
			}
			last, _ := opt.Window(c.TimeRange.MaxTime() - 1)

			// Determine the time difference using the number of buckets.
			// Determine the maximum difference between the buckets based on the end time.
			maxDiff := last - models.MinNanoTime
			if maxDiff/int64(interval) > int64(sopt.MaxBucketsN) {
				timeRange.Min = time.Unix(0, models.MinNanoTime)
			} else {
				timeRange.Min = time.Unix(0, last-int64(interval)*int64(sopt.MaxBucketsN-1))
			}
		}
	}

	// Create an iterator creator based on the shards in the cluster.
	shards, err := shardMapper.MapShards(c.stmt.Sources, timeRange, sopt)
	if err != nil {
		return nil, err
	}

	// Rewrite wildcards, if any exist.
	stmt, err := c.stmt.RewriteFields(shards)
	if err != nil {
		shards.Close()
		return nil, err
	}

	// Determine base options for iterators.
	opt, err := newIteratorOptionsStmt(stmt, sopt)
	if err != nil {
		shards.Close()
		return nil, err
	}
	opt.StartTime, opt.EndTime = c.TimeRange.MinTime(), c.TimeRange.MaxTime()
	opt.Ascending = c.Ascending

	if sopt.MaxBucketsN > 0 && !stmt.IsRawQuery && c.TimeRange.MinTime() > influxql.MinTime {
		interval, err := stmt.GroupByInterval()
		if err != nil {
			shards.Close()
			return nil, err
		}

		if interval > 0 {
			// Determine the start and end time matched to the interval (may not match the actual times).
			first, _ := opt.Window(opt.StartTime)
			last, _ := opt.Window(opt.EndTime - 1)

			// Determine the number of buckets by finding the time span and dividing by the interval.
			buckets := (last - first + int64(interval)) / int64(interval)
			if int(buckets) > sopt.MaxBucketsN {
				shards.Close()
				return nil, fmt.Errorf("max-select-buckets limit exceeded: (%d/%d)", buckets, sopt.MaxBucketsN)
			}
		}
	}

	columns := stmt.ColumnNames()
	return &preparedStatement{
		stmt:    stmt,
		opt:     opt,
		ic:      shards,
		columns: columns,
	}, nil
}
