// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachange"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/cast"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

func alterTableAlterColumnType(
	b BuildCtx, tn *tree.TableName, tbl *scpb.Table, t *tree.AlterTableAlterColumnType,
) {
	colID := getColumnIDFromColumnName(b, tbl.TableID, t.Column, true /* required */)
	col := mustRetrieveColumnElem(b, tbl.TableID, colID)
	panicIfSystemColumn(col, t.Column.String())

	// Setup for the new type ahead of any checking. As we need its resolved type
	// for the checks.
	oldColType := retrieveColumnTypeElem(b, tbl.TableID, colID)
	newColType := *oldColType
	newColType.TypeT = b.ResolveTypeRef(t.ToType)

	// Check for elements depending on the column we are altering.
	walkColumnDependencies(b, col, "alter type of", "column", func(e scpb.Element, op, objType string) {
		switch e := e.(type) {
		case *scpb.Column:
			if e.TableID == col.TableID && e.ColumnID == col.ColumnID {
				return
			}
			elts := b.QueryByID(e.TableID).Filter(hasColumnIDAttrFilter(e.ColumnID))
			computedColName := elts.FilterColumnName().MustGetOneElement()
			panic(sqlerrors.NewDependentBlocksOpError(op, objType, t.Column.String(), "computed column", computedColName.Name))
		case *scpb.View:
			ns := b.QueryByID(col.TableID).FilterNamespace().MustGetOneElement()
			nsDep := b.QueryByID(e.ViewID).FilterNamespace().MustGetOneElement()
			if nsDep.DatabaseID != ns.DatabaseID || nsDep.SchemaID != ns.SchemaID {
				panic(sqlerrors.NewDependentBlocksOpError(op, objType, t.Column.String(), "view", qualifiedName(b, e.ViewID)))
			}
			panic(sqlerrors.NewDependentBlocksOpError(op, objType, t.Column.String(), "view", nsDep.Name))
		case *scpb.FunctionBody:
			fnName := b.QueryByID(e.FunctionID).FilterFunctionName().MustGetOneElement()
			panic(sqlerrors.NewDependentBlocksOpError(op, objType, t.Column.String(), "function", fnName.Name))
		case *scpb.RowLevelTTL:
			// If a duration expression is set, the column level dependency is on the
			// internal ttl column, which we are attempting to alter.
			if e.DurationExpr != "" {
				panic(sqlerrors.NewAlterDependsOnDurationExprError(op, objType, t.Column.String(), tn.Object()))
			}
			// Otherwise, it is a dependency on the column used in the expiration
			// expression.
			panic(sqlerrors.NewAlterDependsOnExpirationExprError(op, objType, t.Column.String(), tn.Object(), string(e.ExpirationExpr)))
		}
	})

	var err error
	newColType.Type, err = schemachange.ValidateAlterColumnTypeChecks(
		b, t, b.ClusterSettings(), newColType.Type,
		col.GeneratedAsIdentityType != catpb.GeneratedAsIdentityType_NOT_IDENTITY_COLUMN)
	if err != nil {
		panic(err)
	}

	validateAutomaticCastForNewType(b, tbl.TableID, colID, t.Column.String(),
		oldColType.Type, newColType.Type, t.Using != nil)

	kind, err := schemachange.ClassifyConversionFromTree(b, t, oldColType.Type, newColType.Type)
	if err != nil {
		panic(err)
	}

	switch kind {
	case schemachange.ColumnConversionTrivial:
		handleTrivialColumnConversion(b, oldColType, &newColType)
	case schemachange.ColumnConversionValidate:
		handleValidationOnlyColumnConversion(b, t, oldColType, &newColType)
	case schemachange.ColumnConversionGeneral:
		handleGeneralColumnConversion(b, t, col, oldColType, &newColType)
	default:
		panic(scerrors.NotImplementedErrorf(t,
			"alter type conversion %v not handled", kind))
	}
}

// ValidateColExprForNewType will ensure that the existing expressions for
// DEFAULT and ON UPDATE will work for the new data type.
func validateAutomaticCastForNewType(
	b BuildCtx,
	tableID catid.DescID,
	colID catid.ColumnID,
	colName string,
	fromType, toType *types.T,
	hasUsingExpr bool,
) {
	if validCast := cast.ValidCast(fromType, toType, cast.ContextAssignment); validCast {
		return
	}

	// If the USING expression is missing, we will report an error with a
	// suggested hint to use one.
	if !hasUsingExpr {
		// Compute a suggested default computed expression for inclusion in the error hint.
		hintExpr := tree.CastExpr{
			Expr:       &tree.ColumnItem{ColumnName: tree.Name(colName)},
			Type:       toType,
			SyntaxMode: tree.CastShort,
		}
		panic(errors.WithHintf(
			pgerror.Newf(
				pgcode.DatatypeMismatch,
				"column %q cannot be cast automatically to type %s",
				colName,
				toType.SQLString(),
			), "You might need to specify \"USING %s\".", tree.Serialize(&hintExpr),
		))
	}

	// We have a USING clause, but if we have DEFAULT or ON UPDATE expressions,
	// then we raise an error because those expressions cannot be automatically
	// cast to the new type.
	columnElements(b, tableID, colID).ForEach(func(
		_ scpb.Status, _ scpb.TargetStatus, e scpb.Element,
	) {
		var exprType string
		switch e.(type) {
		case *scpb.ColumnDefaultExpression:
			exprType = "default"
		case *scpb.ColumnOnUpdateExpression:
			exprType = "on update"
		default:
			return
		}
		panic(pgerror.Newf(
			pgcode.DatatypeMismatch,
			"%s for column %q cannot be cast automatically to type %s",
			exprType,
			colName,
			toType.SQLString(),
		))
	})
}

// handleTrivialColumnConversion is called to just change the type in-place without
// no rewrite or validation required.
func handleTrivialColumnConversion(b BuildCtx, oldColType, newColType *scpb.ColumnType) {
	// Add the new type and remove the old type. The removal of the old type is a
	// no-op in opgen. But we need the drop here so that we only have 1 public
	// type for the column.
	b.Drop(oldColType)
	b.Add(newColType)
}

// handleValidationOnlyColumnConversion is called when we don't need to rewrite
// data, only validate the existing data is compatible with the type.
func handleValidationOnlyColumnConversion(
	b BuildCtx, t *tree.AlterTableAlterColumnType, oldColType, newColType *scpb.ColumnType,
) {
	failIfExperimentalSettingNotSet(b, oldColType, newColType)

	// TODO(spilchen): Implement the validation-only logic in #127516
	panic(scerrors.NotImplementedErrorf(t,
		"alter type conversion that requires validation only is not supported in the declarative schema changer"))
}

// handleGeneralColumnConversion is called when we need to rewrite the data in order
// to complete the data type conversion.
func handleGeneralColumnConversion(
	b BuildCtx,
	t *tree.AlterTableAlterColumnType,
	col *scpb.Column,
	oldColType, newColType *scpb.ColumnType,
) {
	failIfExperimentalSettingNotSet(b, oldColType, newColType)

	// Because we need to rewrite data to change the data type, there are
	// additional validation checks required that are incompatible with this
	// process.
	walkColumnDependencies(b, col, "alter type of", "column", func(e scpb.Element, op, objType string) {
		switch e.(type) {
		case *scpb.SequenceOwner:
			panic(sqlerrors.NewAlterColumnTypeColOwnsSequenceNotSupportedErr())
		case *scpb.CheckConstraint, *scpb.CheckConstraintUnvalidated,
			*scpb.UniqueWithoutIndexConstraint, *scpb.UniqueWithoutIndexConstraintUnvalidated,
			*scpb.ForeignKeyConstraint, *scpb.ForeignKeyConstraintUnvalidated:
			panic(sqlerrors.NewAlterColumnTypeColWithConstraintNotSupportedErr())
		case *scpb.SecondaryIndex:
			panic(sqlerrors.NewAlterColumnTypeColInIndexNotSupportedErr())
		}
	})

	// TODO(spilchen): Implement the general conversion logic in #127014
	panic(scerrors.NotImplementedErrorf(t, "general alter type conversion not supported in the declarative schema changer"))
}

// failIfExperimentalSettingNotSet checks if the setting that allows altering
// types is enabled. If the setting is not enabled, this function will panic.
func failIfExperimentalSettingNotSet(b BuildCtx, oldColType, newColType *scpb.ColumnType) {
	if !b.SessionData().AlterColumnTypeGeneralEnabled {
		panic(pgerror.WithCandidateCode(
			errors.WithHint(
				errors.WithIssueLink(
					errors.Newf("ALTER COLUMN TYPE from %v to %v is only "+
						"supported experimentally",
						oldColType.Type, newColType.Type),
					errors.IssueLink{IssueURL: build.MakeIssueURL(49329)}),
				"you can enable alter column type general support by running "+
					"`SET enable_experimental_alter_column_type_general = true`"),
			pgcode.ExperimentalFeature))
	}
}
