// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"
	"strconv"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/opcode"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	driver "github.com/pingcap/tidb/types/parser_driver"
	"github.com/pingcap/tidb/util/chunk"
)

// EvalSubquery evaluates incorrelated subqueries once.
var EvalSubquery func(ctx context.Context, p PhysicalPlan, is infoschema.InfoSchema, sctx sessionctx.Context) ([][]types.Datum, error)

// evalAstExpr evaluates ast expression directly.
func evalAstExpr(sctx sessionctx.Context, expr ast.ExprNode) (types.Datum, error) {
	if val, ok := expr.(*driver.ValueExpr); ok {
		return val.Datum, nil
	}
	var is infoschema.InfoSchema
	if sctx.GetSessionVars().TxnCtx.InfoSchema != nil {
		is = sctx.GetSessionVars().TxnCtx.InfoSchema.(infoschema.InfoSchema)
	}
	b := NewPlanBuilder(sctx, is)
	fakePlan := LogicalTableDual{}.Init(sctx)
	newExpr, _, err := b.rewrite(context.TODO(), expr, fakePlan, nil, true)
	if err != nil {
		return types.Datum{}, err
	}
	return newExpr.Eval(chunk.Row{})
}

// rewrite function rewrites ast expr to expression.Expression.
// aggMapper maps ast.AggregateFuncExpr to the columns offset in p's output schema.
// asScalar means whether this expression must be treated as a scalar expression.
// And this function returns a result expression, a new plan that may have apply or semi-join.
func (b *PlanBuilder) rewrite(ctx context.Context, exprNode ast.ExprNode, p LogicalPlan, aggMapper map[*ast.AggregateFuncExpr]int, asScalar bool) (expression.Expression, LogicalPlan, error) {
	expr, resultPlan, err := b.rewriteWithPreprocess(ctx, exprNode, p, aggMapper, asScalar, nil)
	return expr, resultPlan, err
}

// rewriteWithPreprocess is for handling the situation that we need to adjust the input ast tree
// before really using its node in `expressionRewriter.Leave`. In that case, we first call
// er.preprocess(expr), which returns a new expr. Then we use the new expr in `Leave`.
func (b *PlanBuilder) rewriteWithPreprocess(
	ctx context.Context,
	exprNode ast.ExprNode,
	p LogicalPlan, aggMapper map[*ast.AggregateFuncExpr]int,
	asScalar bool,
	preprocess func(ast.Node) ast.Node,
) (expression.Expression, LogicalPlan, error) {
	b.rewriterCounter++
	defer func() { b.rewriterCounter-- }()

	rewriter := b.getExpressionRewriter(ctx, p)
	// The rewriter maybe is obtained from "b.rewriterPool", "rewriter.err" is
	// not nil means certain previous procedure has not handled this error.
	// Here we give us one more chance to make a correct behavior by handling
	// this missed error.
	if rewriter.err != nil {
		return nil, nil, rewriter.err
	}

	rewriter.aggrMap = aggMapper
	rewriter.asScalar = asScalar
	rewriter.preprocess = preprocess

	expr, resultPlan, err := b.rewriteExprNode(rewriter, exprNode, asScalar)
	return expr, resultPlan, err
}

func (b *PlanBuilder) getExpressionRewriter(ctx context.Context, p LogicalPlan) (rewriter *expressionRewriter) {
	defer func() {
		if p != nil {
			rewriter.schema = p.Schema()
			rewriter.names = p.OutputNames()
		}
	}()

	if len(b.rewriterPool) < b.rewriterCounter {
		rewriter = &expressionRewriter{p: p, b: b, sctx: b.ctx, ctx: ctx}
		b.rewriterPool = append(b.rewriterPool, rewriter)
		return
	}

	rewriter = b.rewriterPool[b.rewriterCounter-1]
	rewriter.p = p
	rewriter.asScalar = false
	rewriter.aggrMap = nil
	rewriter.preprocess = nil
	rewriter.insertPlan = nil
	rewriter.disableFoldCounter = 0
	rewriter.ctxStack = rewriter.ctxStack[:0]
	rewriter.ctxNameStk = rewriter.ctxNameStk[:0]
	rewriter.ctx = ctx
	return
}

func (b *PlanBuilder) rewriteExprNode(rewriter *expressionRewriter, exprNode ast.ExprNode, asScalar bool) (expression.Expression, LogicalPlan, error) {
	if rewriter.p != nil {
		curColLen := rewriter.p.Schema().Len()
		defer func() {
			names := rewriter.p.OutputNames().Shallow()[:curColLen]
			for i := curColLen; i < rewriter.p.Schema().Len(); i++ {
				names = append(names, types.EmptyName)
			}
			// After rewriting finished, only old columns are visible.
			// e.g. select * from t where t.a in (select t1.a from t1);
			// The output columns before we enter the subquery are the columns from t.
			// But when we leave the subquery `t.a in (select t1.a from t1)`, we got a Apply operator
			// and the output columns become [t.*, t1.*]. But t1.* is used only inside the subquery. If there's another filter
			// which is also a subquery where t1 is involved. The name resolving will fail if we still expose the column from
			// the previous subquery.
			// So here we just reset the names to empty to avoid this situation.
			// TODO: implement ScalarSubQuery and resolve it during optimizing. In building phase, we will not change the plan's structure.
			rewriter.p.SetOutputNames(names)
		}()
	}
	exprNode.Accept(rewriter)
	if rewriter.err != nil {
		return nil, nil, errors.Trace(rewriter.err)
	}
	if !asScalar && len(rewriter.ctxStack) == 0 {
		return nil, rewriter.p, nil
	}
	if len(rewriter.ctxStack) != 1 {
		return nil, nil, errors.Errorf("context len %v is invalid", len(rewriter.ctxStack))
	}
	rewriter.err = expression.CheckArgsNotMultiColumnRow(rewriter.ctxStack[0])
	if rewriter.err != nil {
		return nil, nil, errors.Trace(rewriter.err)
	}
	return rewriter.ctxStack[0], rewriter.p, nil
}

type expressionRewriter struct {
	ctxStack   []expression.Expression
	ctxNameStk []*types.FieldName
	p          LogicalPlan
	schema     *expression.Schema
	names      []*types.FieldName
	err        error
	aggrMap    map[*ast.AggregateFuncExpr]int
	b          *PlanBuilder
	sctx       sessionctx.Context
	ctx        context.Context

	// asScalar indicates the return value must be a scalar value.
	// NOTE: This value can be changed during expression rewritten.
	asScalar bool

	// preprocess is called for every ast.Node in Leave.
	preprocess func(ast.Node) ast.Node

	// insertPlan is only used to rewrite the expressions inside the assignment
	// of the "INSERT" statement.
	insertPlan *Insert

	// disableFoldCounter controls fold-disabled scope. If > 0, rewriter will NOT do constant folding.
	// Typically, during visiting AST, while entering the scope(disable), the counter will +1; while
	// leaving the scope(enable again), the counter will -1.
	// NOTE: This value can be changed during expression rewritten.
	disableFoldCounter int
}

func (er *expressionRewriter) ctxStackLen() int {
	return len(er.ctxStack)
}

func (er *expressionRewriter) ctxStackPop(num int) {
	l := er.ctxStackLen()
	er.ctxStack = er.ctxStack[:l-num]
	er.ctxNameStk = er.ctxNameStk[:l-num]
}

func (er *expressionRewriter) ctxStackAppend(col expression.Expression, name *types.FieldName) {
	er.ctxStack = append(er.ctxStack, col)
	er.ctxNameStk = append(er.ctxNameStk, name)
}

// constructBinaryOpFunction converts binary operator functions
// 1. If op are EQ or NE or NullEQ, constructBinaryOpFunctions converts (a0,a1,a2) op (b0,b1,b2) to (a0 op b0) and (a1 op b1) and (a2 op b2)
// 2. Else constructBinaryOpFunctions converts (a0,a1,a2) op (b0,b1,b2) to
// `IF( a0 NE b0, a0 op b0,
// 		IF ( isNull(a0 NE b0), Null,
// 			IF ( a1 NE b1, a1 op b1,
// 				IF ( isNull(a1 NE b1), Null, a2 op b2))))`
func (er *expressionRewriter) constructBinaryOpFunction(l expression.Expression, r expression.Expression, op string) (expression.Expression, error) {
	lLen, rLen := expression.GetRowLen(l), expression.GetRowLen(r)
	if lLen == 1 && rLen == 1 {
		return er.newFunction(op, types.NewFieldType(mysql.TypeTiny), l, r)
	} else if rLen != lLen {
		return nil, expression.ErrOperandColumns.GenWithStackByArgs(lLen)
	}
	switch op {
	case ast.EQ, ast.NE, ast.NullEQ:
		funcs := make([]expression.Expression, lLen)
		for i := 0; i < lLen; i++ {
			var err error
			funcs[i], err = er.constructBinaryOpFunction(expression.GetFuncArg(l, i), expression.GetFuncArg(r, i), op)
			if err != nil {
				return nil, err
			}
		}
		if op == ast.NE {
			return expression.ComposeDNFCondition(er.sctx, funcs...), nil
		}
		return expression.ComposeCNFCondition(er.sctx, funcs...), nil
	default:
		larg0, rarg0 := expression.GetFuncArg(l, 0), expression.GetFuncArg(r, 0)
		var expr1, expr2, expr3, expr4, expr5 expression.Expression
		expr1 = expression.NewFunctionInternal(er.sctx, ast.NE, types.NewFieldType(mysql.TypeTiny), larg0, rarg0)
		expr2 = expression.NewFunctionInternal(er.sctx, op, types.NewFieldType(mysql.TypeTiny), larg0, rarg0)
		expr3 = expression.NewFunctionInternal(er.sctx, ast.IsNull, types.NewFieldType(mysql.TypeTiny), expr1)
		var err error
		l, err = expression.PopRowFirstArg(er.sctx, l)
		if err != nil {
			return nil, err
		}
		r, err = expression.PopRowFirstArg(er.sctx, r)
		if err != nil {
			return nil, err
		}
		expr4, err = er.constructBinaryOpFunction(l, r, op)
		if err != nil {
			return nil, err
		}
		expr5, err = er.newFunction(ast.If, types.NewFieldType(mysql.TypeTiny), expr3, expression.Null, expr4)
		if err != nil {
			return nil, err
		}
		return er.newFunction(ast.If, types.NewFieldType(mysql.TypeTiny), expr1, expr2, expr5)
	}
}

// Enter implements Visitor interface.
func (er *expressionRewriter) Enter(inNode ast.Node) (ast.Node, bool) {
	switch v := inNode.(type) {
	case *ast.AggregateFuncExpr:
		index, ok := -1, false
		if er.aggrMap != nil {
			index, ok = er.aggrMap[v]
		}
		if !ok {
			er.err = ErrInvalidGroupFuncUse
			return inNode, true
		}
		er.ctxStackAppend(er.schema.Columns[index], er.names[index])
		return inNode, true
	case *ast.ColumnNameExpr:
		if index, ok := er.b.colMapper[v]; ok {
			er.ctxStackAppend(er.schema.Columns[index], er.names[index])
			return inNode, true
		}
	case *ast.PatternInExpr:
		if len(v.List) != 1 {
			break
		}
		// For 10 in ((select * from t)), the parser won't set v.Sel.
		// So we must process this case here.
		x := v.List[0]
		for {
			switch y := x.(type) {
			case *ast.ParenthesesExpr:
				x = y.Expr
			default:
				return inNode, false
			}
		}
	case *ast.ParenthesesExpr:
	case *ast.ValuesExpr:
		schema, names := er.schema, er.names
		// NOTE: "er.insertPlan != nil" means that we are rewriting the
		// expressions inside the assignment of "INSERT" statement. we have to
		// use the "tableSchema" of that "insertPlan".
		if er.insertPlan != nil {
			schema = er.insertPlan.tableSchema
			names = er.insertPlan.tableColNames
		}
		idx, err := expression.FindFieldName(names, v.Column.Name)
		if err != nil {
			er.err = err
			return inNode, false
		}
		if idx < 0 {
			er.err = ErrUnknownColumn.GenWithStackByArgs(v.Column.Name.OrigColName(), "field list")
			return inNode, false
		}
		col := schema.Columns[idx]
		er.ctxStackAppend(expression.NewValuesFunc(er.sctx, col.Index, col.RetType), types.EmptyName)
		return inNode, true
	case *ast.FuncCallExpr:
		if _, ok := expression.DisableFoldFunctions[v.FnName.L]; ok {
			er.disableFoldCounter++
		}
	default:
		er.asScalar = true
	}
	return inNode, false
}

// Leave implements Visitor interface.
func (er *expressionRewriter) Leave(originInNode ast.Node) (retNode ast.Node, ok bool) {
	if er.err != nil {
		return retNode, false
	}
	var inNode = originInNode
	if er.preprocess != nil {
		inNode = er.preprocess(inNode)
	}
	switch v := inNode.(type) {
	case *ast.AggregateFuncExpr, *ast.ColumnNameExpr, *ast.ParenthesesExpr, *ast.WhenClause,
		*ast.SubqueryExpr, *ast.ExistsSubqueryExpr, *ast.CompareSubqueryExpr, *ast.ValuesExpr:
	case *driver.ValueExpr:
		value := &expression.Constant{Value: v.Datum, RetType: &v.Type}
		er.ctxStackAppend(value, types.EmptyName)
	case *driver.ParamMarkerExpr:
		var value expression.Expression
		value, er.err = expression.ParamMarkerExpression(er.sctx, v)
		if er.err != nil {
			return retNode, false
		}
		er.ctxStackAppend(value, types.EmptyName)
	case *ast.VariableExpr:
		er.rewriteVariable(v)
	case *ast.FuncCallExpr:
		er.funcCallToExpression(v)
		if _, ok := expression.DisableFoldFunctions[v.FnName.L]; ok {
			er.disableFoldCounter--
		}
	case *ast.ColumnName:
		er.toColumn(v)
	case *ast.UnaryOperationExpr:
		er.unaryOpToExpression(v)
	case *ast.BinaryOperationExpr:
		er.binaryOpToExpression(v)
	case *ast.BetweenExpr:
		er.betweenToExpression(v)
	case *ast.CaseExpr:
		er.caseToExpression(v)
	case *ast.RowExpr:
		er.rowToScalarFunc(v)
	case *ast.PatternInExpr:
		if v.Sel == nil {
			er.inToExpression(len(v.List), v.Not, &v.Type)
		}
	case *ast.PositionExpr:
		er.positionToScalarFunc(v)
	case *ast.IsNullExpr:
		er.isNullToExpression(v)
	case *ast.IsTruthExpr:
		er.isTrueToScalarFunc(v)
	case *ast.DefaultExpr:
		er.evalDefaultExpr(v)
	// TODO: Perhaps we don't need to transcode these back to generic integers/strings
	case *ast.TrimDirectionExpr:
		er.ctxStackAppend(&expression.Constant{
			Value:   types.NewIntDatum(int64(v.Direction)),
			RetType: types.NewFieldType(mysql.TypeTiny),
		}, types.EmptyName)
	case *ast.TimeUnitExpr:
		er.ctxStackAppend(&expression.Constant{
			Value:   types.NewStringDatum(v.Unit.String()),
			RetType: types.NewFieldType(mysql.TypeVarchar),
		}, types.EmptyName)
	case *ast.GetFormatSelectorExpr:
		er.ctxStackAppend(&expression.Constant{
			Value:   types.NewStringDatum(v.Selector.String()),
			RetType: types.NewFieldType(mysql.TypeVarchar),
		}, types.EmptyName)
	default:
		er.err = errors.Errorf("UnknownType: %T", v)
		return retNode, false
	}

	if er.err != nil {
		return retNode, false
	}
	return originInNode, true
}

// newFunction chooses which expression.NewFunctionImpl() will be used.
func (er *expressionRewriter) newFunction(funcName string, retType *types.FieldType, args ...expression.Expression) (expression.Expression, error) {
	if er.disableFoldCounter > 0 {
		return expression.NewFunctionBase(er.sctx, funcName, retType, args...)
	}
	return expression.NewFunction(er.sctx, funcName, retType, args...)
}

func (er *expressionRewriter) rewriteVariable(v *ast.VariableExpr) {
	stkLen := len(er.ctxStack)
	name := strings.ToLower(v.Name)
	sessionVars := er.b.ctx.GetSessionVars()
	if !v.IsSystem {
		if v.Value != nil {
			er.ctxStack[stkLen-1], er.err = er.newFunction(ast.SetVar,
				er.ctxStack[stkLen-1].GetType(),
				expression.DatumToConstant(types.NewDatum(name), mysql.TypeString),
				er.ctxStack[stkLen-1])
			er.ctxNameStk[stkLen-1] = types.EmptyName
			return
		}
		f, err := er.newFunction(ast.GetVar,
			// TODO: Here is wrong, the sessionVars should store a name -> Datum map. Will fix it later.
			types.NewFieldType(mysql.TypeString),
			expression.DatumToConstant(types.NewStringDatum(name), mysql.TypeString))
		if err != nil {
			er.err = err
			return
		}
		er.ctxStackAppend(f, types.EmptyName)
		return
	}
	var val string
	var err error
	if v.ExplicitScope {
		err = variable.ValidateGetSystemVar(name, v.IsGlobal)
		if err != nil {
			er.err = err
			return
		}
	}
	sysVar := variable.SysVars[name]
	if sysVar == nil {
		er.err = variable.ErrUnknownSystemVar.GenWithStackByArgs(name)
		return
	}
	// Variable is @@gobal.variable_name or variable is only global scope variable.
	if v.IsGlobal || sysVar.Scope == variable.ScopeGlobal {
		val, err = variable.GetGlobalSystemVar(sessionVars, name)
	} else {
		val, err = variable.GetSessionSystemVar(sessionVars, name)
	}
	if err != nil {
		er.err = err
		return
	}
	e := expression.DatumToConstant(types.NewStringDatum(val), mysql.TypeVarString)
	e.GetType().Charset, _ = er.sctx.GetSessionVars().GetSystemVar(variable.CharacterSetConnection)
	e.GetType().Collate, _ = er.sctx.GetSessionVars().GetSystemVar(variable.CollationConnection)
	er.ctxStackAppend(e, types.EmptyName)
}

func (er *expressionRewriter) unaryOpToExpression(v *ast.UnaryOperationExpr) {
	stkLen := len(er.ctxStack)
	var op string
	switch v.Op {
	case opcode.Plus:
		// expression (+ a) is equal to a
		return
	case opcode.Minus:
		op = ast.UnaryMinus
	case opcode.BitNeg:
		op = ast.BitNeg
	case opcode.Not:
		op = ast.UnaryNot
	default:
		er.err = errors.Errorf("Unknown Unary Op %T", v.Op)
		return
	}
	if expression.GetRowLen(er.ctxStack[stkLen-1]) != 1 {
		er.err = expression.ErrOperandColumns.GenWithStackByArgs(1)
		return
	}
	er.ctxStack[stkLen-1], er.err = er.newFunction(op, &v.Type, er.ctxStack[stkLen-1])
	er.ctxNameStk[stkLen-1] = types.EmptyName
}

func (er *expressionRewriter) binaryOpToExpression(v *ast.BinaryOperationExpr) {
	stkLen := len(er.ctxStack)
	var function expression.Expression
	switch v.Op {
	case opcode.EQ, opcode.NE, opcode.NullEQ, opcode.GT, opcode.GE, opcode.LT, opcode.LE:
		function, er.err = er.constructBinaryOpFunction(er.ctxStack[stkLen-2], er.ctxStack[stkLen-1],
			v.Op.String())
	default:
		lLen := expression.GetRowLen(er.ctxStack[stkLen-2])
		rLen := expression.GetRowLen(er.ctxStack[stkLen-1])
		if lLen != 1 || rLen != 1 {
			er.err = expression.ErrOperandColumns.GenWithStackByArgs(1)
			return
		}
		function, er.err = er.newFunction(v.Op.String(), types.NewFieldType(mysql.TypeUnspecified), er.ctxStack[stkLen-2:]...)
	}
	if er.err != nil {
		return
	}
	er.ctxStackPop(2)
	er.ctxStackAppend(function, types.EmptyName)
}

func (er *expressionRewriter) notToExpression(hasNot bool, op string, tp *types.FieldType,
	args ...expression.Expression) expression.Expression {
	opFunc, err := er.newFunction(op, tp, args...)
	if err != nil {
		er.err = err
		return nil
	}
	if !hasNot {
		return opFunc
	}

	opFunc, err = er.newFunction(ast.UnaryNot, tp, opFunc)
	if err != nil {
		er.err = err
		return nil
	}
	return opFunc
}

func (er *expressionRewriter) isNullToExpression(v *ast.IsNullExpr) {
	stkLen := len(er.ctxStack)
	if expression.GetRowLen(er.ctxStack[stkLen-1]) != 1 {
		er.err = expression.ErrOperandColumns.GenWithStackByArgs(1)
		return
	}
	function := er.notToExpression(v.Not, ast.IsNull, &v.Type, er.ctxStack[stkLen-1])
	er.ctxStackPop(1)
	er.ctxStackAppend(function, types.EmptyName)
}

func (er *expressionRewriter) positionToScalarFunc(v *ast.PositionExpr) {
	pos := v.N
	str := strconv.Itoa(pos)
	if v.P != nil {
		stkLen := len(er.ctxStack)
		val := er.ctxStack[stkLen-1]
		intNum, isNull, err := expression.GetIntFromConstant(er.sctx, val)
		str = "?"
		if err == nil {
			if isNull {
				return
			}
			pos = intNum
			er.ctxStackPop(1)
		}
		er.err = err
	}
	if er.err == nil && pos > 0 && pos <= er.schema.Len() {
		er.ctxStackAppend(er.schema.Columns[pos-1], er.names[pos-1])
	} else {
		er.err = ErrUnknownColumn.GenWithStackByArgs(str, clauseMsg[er.b.curClause])
	}
}

func (er *expressionRewriter) isTrueToScalarFunc(v *ast.IsTruthExpr) {
	stkLen := len(er.ctxStack)
	op := ast.IsTruth
	if v.True == 0 {
		op = ast.IsFalsity
	}
	if expression.GetRowLen(er.ctxStack[stkLen-1]) != 1 {
		er.err = expression.ErrOperandColumns.GenWithStackByArgs(1)
		return
	}
	function := er.notToExpression(v.Not, op, &v.Type, er.ctxStack[stkLen-1])
	er.ctxStackPop(1)
	er.ctxStackAppend(function, types.EmptyName)
}

// inToExpression converts in expression to a scalar function. The argument lLen means the length of in list.
// The argument not means if the expression is not in. The tp stands for the expression type, which is always bool.
// a in (b, c, d) will be rewritten as `(a = b) or (a = c) or (a = d)`.
func (er *expressionRewriter) inToExpression(lLen int, not bool, tp *types.FieldType) {
	stkLen := len(er.ctxStack)
	l := expression.GetRowLen(er.ctxStack[stkLen-lLen-1])
	for i := 0; i < lLen; i++ {
		if l != expression.GetRowLen(er.ctxStack[stkLen-lLen+i]) {
			er.err = expression.ErrOperandColumns.GenWithStackByArgs(l)
			return
		}
	}
	args := er.ctxStack[stkLen-lLen-1:]
	leftFt := args[0].GetType()
	leftEt, leftIsNull := leftFt.EvalType(), leftFt.Tp == mysql.TypeNull
	if leftIsNull {
		er.ctxStackPop(lLen + 1)
		er.ctxStackAppend(expression.Null.Clone(), types.EmptyName)
		return
	}
	if leftEt == types.ETInt {
		for i := 1; i < len(args); i++ {
			if c, ok := args[i].(*expression.Constant); ok {
				var isExceptional bool
				args[i], isExceptional = expression.RefineComparedConstant(er.sctx, *leftFt, c, opcode.EQ)
				if isExceptional {
					args[i] = c
				}
			}
		}
	}
	allSameType := true
	for _, arg := range args[1:] {
		if arg.GetType().Tp != mysql.TypeNull && expression.GetAccurateCmpType(args[0], arg) != leftEt {
			allSameType = false
			break
		}
	}
	var function expression.Expression
	if allSameType && l == 1 && lLen > 1 {
		function = er.notToExpression(not, ast.In, tp, er.ctxStack[stkLen-lLen-1:]...)
	} else {
		eqFunctions := make([]expression.Expression, 0, lLen)
		for i := stkLen - lLen; i < stkLen; i++ {
			expr, err := er.constructBinaryOpFunction(args[0], er.ctxStack[i], ast.EQ)
			if err != nil {
				er.err = err
				return
			}
			eqFunctions = append(eqFunctions, expr)
		}
		function = expression.ComposeDNFCondition(er.sctx, eqFunctions...)
		if not {
			var err error
			function, err = er.newFunction(ast.UnaryNot, tp, function)
			if err != nil {
				er.err = err
				return
			}
		}
	}
	er.ctxStackPop(lLen + 1)
	er.ctxStackAppend(function, types.EmptyName)
}

func (er *expressionRewriter) caseToExpression(v *ast.CaseExpr) {
	stkLen := len(er.ctxStack)
	argsLen := 2 * len(v.WhenClauses)
	if v.ElseClause != nil {
		argsLen++
	}
	er.err = expression.CheckArgsNotMultiColumnRow(er.ctxStack[stkLen-argsLen:]...)
	if er.err != nil {
		return
	}

	// value                          -> ctxStack[stkLen-argsLen-1]
	// when clause(condition, result) -> ctxStack[stkLen-argsLen:stkLen-1];
	// else clause                    -> ctxStack[stkLen-1]
	var args []expression.Expression
	if v.Value != nil {
		// args:  eq scalar func(args: value, condition1), result1,
		//        eq scalar func(args: value, condition2), result2,
		//        ...
		//        else clause
		value := er.ctxStack[stkLen-argsLen-1]
		args = make([]expression.Expression, 0, argsLen)
		for i := stkLen - argsLen; i < stkLen-1; i += 2 {
			arg, err := er.newFunction(ast.EQ, types.NewFieldType(mysql.TypeTiny), value, er.ctxStack[i])
			if err != nil {
				er.err = err
				return
			}
			args = append(args, arg)
			args = append(args, er.ctxStack[i+1])
		}
		if v.ElseClause != nil {
			args = append(args, er.ctxStack[stkLen-1])
		}
		argsLen++ // for trimming the value element later
	} else {
		// args:  condition1, result1,
		//        condition2, result2,
		//        ...
		//        else clause
		args = er.ctxStack[stkLen-argsLen:]
	}
	function, err := er.newFunction(ast.Case, &v.Type, args...)
	if err != nil {
		er.err = err
		return
	}
	er.ctxStackPop(argsLen)
	er.ctxStackAppend(function, types.EmptyName)
}

func (er *expressionRewriter) rowToScalarFunc(v *ast.RowExpr) {
	stkLen := len(er.ctxStack)
	length := len(v.Values)
	rows := make([]expression.Expression, 0, length)
	for i := stkLen - length; i < stkLen; i++ {
		rows = append(rows, er.ctxStack[i])
	}
	er.ctxStackPop(length)
	function, err := er.newFunction(ast.RowFunc, rows[0].GetType(), rows...)
	if err != nil {
		er.err = err
		return
	}
	er.ctxStackAppend(function, types.EmptyName)
}

func (er *expressionRewriter) betweenToExpression(v *ast.BetweenExpr) {
	stkLen := len(er.ctxStack)
	er.err = expression.CheckArgsNotMultiColumnRow(er.ctxStack[stkLen-3:]...)
	if er.err != nil {
		return
	}

	expr, lexp, rexp := er.ctxStack[stkLen-3], er.ctxStack[stkLen-2], er.ctxStack[stkLen-1]

	var op string
	var l, r expression.Expression
	l, er.err = er.newFunction(ast.GE, &v.Type, expr, lexp)
	if er.err == nil {
		r, er.err = er.newFunction(ast.LE, &v.Type, expr, rexp)
	}
	op = ast.LogicAnd
	if er.err != nil {
		return
	}
	function, err := er.newFunction(op, &v.Type, l, r)
	if err != nil {
		er.err = err
		return
	}
	if v.Not {
		function, err = er.newFunction(ast.UnaryNot, &v.Type, function)
		if err != nil {
			er.err = err
			return
		}
	}
	er.ctxStackPop(3)
	er.ctxStackAppend(function, types.EmptyName)
}

// rewriteFuncCall handles a FuncCallExpr and generates a customized function.
// It should return true if for the given FuncCallExpr a rewrite is performed so that original behavior is skipped.
// Otherwise it should return false to indicate (the caller) that original behavior needs to be performed.
func (er *expressionRewriter) rewriteFuncCall(v *ast.FuncCallExpr) bool {
	switch v.FnName.L {
	// when column is not null, ifnull on such column is not necessary.
	case ast.Ifnull:
		if len(v.Args) != 2 {
			er.err = expression.ErrIncorrectParameterCount.GenWithStackByArgs(v.FnName.O)
			return true
		}
		stackLen := len(er.ctxStack)
		arg1 := er.ctxStack[stackLen-2]
		col, isColumn := arg1.(*expression.Column)
		// if expr1 is a column and column has not null flag, then we can eliminate ifnull on
		// this column.
		if isColumn && mysql.HasNotNullFlag(col.RetType.Flag) {
			name := er.ctxNameStk[stackLen-2]
			newCol := col.Clone().(*expression.Column)
			er.ctxStackPop(len(v.Args))
			er.ctxStackAppend(newCol, name)
			return true
		}

		return false
	case ast.Nullif:
		if len(v.Args) != 2 {
			er.err = expression.ErrIncorrectParameterCount.GenWithStackByArgs(v.FnName.O)
			return true
		}
		stackLen := len(er.ctxStack)
		param1 := er.ctxStack[stackLen-2]
		param2 := er.ctxStack[stackLen-1]
		// param1 = param2
		funcCompare, err := er.constructBinaryOpFunction(param1, param2, ast.EQ)
		if err != nil {
			er.err = err
			return true
		}
		// NULL
		nullTp := types.NewFieldType(mysql.TypeNull)
		nullTp.Flen, nullTp.Decimal = mysql.GetDefaultFieldLengthAndDecimal(mysql.TypeNull)
		paramNull := &expression.Constant{
			Value:   types.NewDatum(nil),
			RetType: nullTp,
		}
		// if(param1 = param2, NULL, param1)
		funcIf, err := er.newFunction(ast.If, &v.Type, funcCompare, paramNull, param1)
		if err != nil {
			er.err = err
			return true
		}
		er.ctxStackPop(len(v.Args))
		er.ctxStackAppend(funcIf, types.EmptyName)
		return true
	default:
		return false
	}
}

func (er *expressionRewriter) funcCallToExpression(v *ast.FuncCallExpr) {
	stackLen := len(er.ctxStack)
	args := er.ctxStack[stackLen-len(v.Args):]
	er.err = expression.CheckArgsNotMultiColumnRow(args...)
	if er.err != nil {
		return
	}

	if er.rewriteFuncCall(v) {
		return
	}

	var function expression.Expression
	er.ctxStackPop(len(v.Args))
	function, er.err = er.newFunction(v.FnName.L, &v.Type, args...)
	er.ctxStackAppend(function, types.EmptyName)
}

func (er *expressionRewriter) toColumn(v *ast.ColumnName) {
	idx, err := expression.FindFieldName(er.names, v)
	if err != nil {
		er.err = ErrAmbiguous.GenWithStackByArgs(v.Name, clauseMsg[fieldList])
		return
	}
	if idx >= 0 {
		column := er.schema.Columns[idx]
		er.ctxStackAppend(column, er.names[idx])
		return
	}
	for i := len(er.b.outerSchemas) - 1; i >= 0; i-- {
		outerSchema, outerName := er.b.outerSchemas[i], er.b.outerNames[i]
		idx, err = expression.FindFieldName(outerName, v)
		if idx >= 0 {
			column := outerSchema.Columns[idx]
			er.ctxStackAppend(&expression.CorrelatedColumn{Column: *column, Data: new(types.Datum)}, outerName[idx])
			return
		}
		if err != nil {
			er.err = ErrAmbiguous.GenWithStackByArgs(v.Name, clauseMsg[fieldList])
			return
		}
	}
	if join, ok := er.p.(*LogicalJoin); ok && join.redundantSchema != nil {
		idx, err := expression.FindFieldName(join.redundantNames, v)
		if err != nil {
			er.err = err
			return
		}
		if idx >= 0 {
			er.ctxStackAppend(join.redundantSchema.Columns[idx], join.redundantNames[idx])
			return
		}
	}
	if er.b.curClause == globalOrderByClause {
		er.b.curClause = orderByClause
	}
	er.err = ErrUnknownColumn.GenWithStackByArgs(v.String(), clauseMsg[er.b.curClause])
}

func (er *expressionRewriter) evalDefaultExpr(v *ast.DefaultExpr) {
	stkLen := len(er.ctxStack)
	name := er.ctxNameStk[stkLen-1]
	switch er.ctxStack[stkLen-1].(type) {
	case *expression.Column:
	case *expression.CorrelatedColumn:
	default:
		idx, err := expression.FindFieldName(er.names, v.Name)
		if err != nil {
			er.err = err
			return
		}
		if er.err != nil {
			return
		}
		if idx < 0 {
			er.err = ErrUnknownColumn.GenWithStackByArgs(v.Name.OrigColName(), "field_list")
			return
		}
	}
	dbName := name.DBName
	if dbName.O == "" {
		// if database name is not specified, use current database name
		dbName = model.NewCIStr(er.sctx.GetSessionVars().CurrentDB)
	}
	if name.OrigTblName.O == "" {
		// column is evaluated by some expressions, for example:
		// `select default(c) from (select (a+1) as c from t) as t0`
		// in such case, a 'no default' error is returned
		er.err = table.ErrNoDefaultValue.GenWithStackByArgs(name.ColName)
		return
	}
	var tbl table.Table
	tbl, er.err = er.b.is.TableByName(dbName, name.OrigTblName)
	if er.err != nil {
		return
	}
	colName := name.OrigColName.O
	if colName == "" {
		// in some cases, OrigColName is empty, use ColName instead
		colName = name.ColName.O
	}
	col := table.FindCol(tbl.Cols(), colName)
	if col == nil {
		er.err = ErrUnknownColumn.GenWithStackByArgs(v.Name, "field_list")
		return
	}
	var val *expression.Constant
	// for other columns, just use what it is
	val, er.err = er.b.getDefaultValue(col)
	if er.err != nil {
		return
	}
	er.ctxStackPop(1)
	er.ctxStackAppend(val, types.EmptyName)
}
