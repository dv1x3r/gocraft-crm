package sqlitedb

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"gostart-crm/internal/app/model"
	"gostart-crm/internal/app/storage"
	"gostart-crm/internal/app/utils"

	"github.com/huandu/go-sqlbuilder"
	"github.com/jmoiron/sqlx"
)

type Product struct {
	db *sqlx.DB
}

func NewProduct(db *sqlx.DB) *Product {
	return &Product{db: db}
}

func (st *Product) getQuerySelectBase(extra ...string) *sqlbuilder.SelectBuilder {
	columns := []string{
		"p.id",
		"p.code",
		"p.name",
		"p.description",
		"p.quantity",
		"p.price",
		"p.is_published",
		"datetime(p.created_at, 'unixepoch', 'localtime') as created_at",
		"datetime(p.updated_at, 'unixepoch', 'localtime') as updated_at",
		"p.category_id",
		"c_cte.name as category_hierarchy",
		"c.attribute_group_id as category_attribute_group_id",
		"p.supplier_id",
		"s.name as supplier_name",
		"p.brand_id",
		"b.name as brand_name",
		"p.status_id",
		"ps.name as status_name",
		"ps.color as status_color",
		"concat(s.slug, '/', p.slug, '/') as product_url",
		"iif(p.quantity > 0 and p.is_published = 1 and c.is_published = 1 and s.is_published = 1, 1, 0) as is_available",
	}

	sb := sqlbuilder.NewSelectBuilder()
	sb.SQL(CategoryCTE)
	sb.Select(append(columns, extra...)...).From("product as p")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category as c", "c.id = p.category_id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category as c_root", "c_root.mp_level = 0 and c.mp_path like c_root.mp_path || '%'")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category_cte as c_cte", "c_cte.id = p.category_id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "supplier as s", "s.id = p.supplier_id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "brand as b", "b.id = p.brand_id")
	sb.JoinWithOption(sqlbuilder.LeftJoin, "product_status as ps", "ps.id = p.status_id")
	return sb
}

func (st *Product) getQueryFindMany(q storage.FindManyParams, categoryID int64) (string, []any) {
	sb := st.getQuerySelectBase("count(*) over () as count")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category as c_selected", "c.mp_path like c_selected.mp_path || '%'")

	if categoryID == 0 {
		sb.Where("c_selected.parent_id is null")
	} else {
		sb.Where(sb.EQ("c_selected.id", categoryID))
	}

	allowedFilters := map[string]string{
		"code":         "p.code",
		"name":         "p.name",
		"category":     "p.category_id",
		"supplier":     "p.supplier_id",
		"brand":        "p.brand_id",
		"status":       "coalesce(p.status_id, 0)",
		"is_published": "p.is_published",
		"quantity":     "p.quantity",
		"price":        "p.price",
	}

	allowedSorters := map[string]string{
		"code":         "p.code",
		"name":         "p.name",
		"supplier":     "s.name",
		"brand":        "b.name",
		"status":       "ps.name",
		"quantity":     "p.quantity",
		"price":        "p.price",
		"is_published": "p.is_published",
		"created_at":   "p.created_at",
		"updated_at":   "p.updated_at",
	}

	storage.ApplyLimitOffset(sb, q.Limit, q.Offset)
	storage.ApplyFilters(sb, q.Filters, q.LogicAnd, allowedFilters)
	storage.ApplySorters(sb, q.Sorters, allowedSorters)

	return sb.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) FindMany(ctx context.Context, q storage.FindManyParams, categoryID int64) ([]model.Product, int64, error) {
	const op = "sqlitedb.Product.FindMany"

	type LocalResult struct {
		model.Product
		Count int64 `db:"count"`
	}

	query, args := st.getQueryFindMany(q, categoryID)
	rows, err := runSelect[LocalResult](ctx, st.db, query, args)
	if err != nil {
		return nil, 0, utils.WrapIfErr(op, err)
	}

	dto, count := make([]model.Product, len(rows)), int64(0)
	for i, row := range rows {
		dto[i], count = row.Product, row.Count
		if err = st.fillDtoDetails(ctx, &dto[i]); err != nil {
			return dto, count, utils.WrapIfErr(op, err)
		}
	}

	return dto, count, nil
}

func (st *Product) getQueryFindAvailableByCategoryID(categoryID int64, filters model.FilterCombination) (string, []any) {
	sb := st.getQuerySelectBase()
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category as c_selected", "c.mp_path like c_selected.mp_path || '%'")
	sb.Where("p.quantity > 0 and p.is_published = 1 and c.is_published = 1 and s.is_published = 1")
	sb.Where(sb.EQ("c_selected.id", categoryID))

	for filterKey, facets := range filters {
		facetKeys := slices.Collect(maps.Keys(facets))
		if filterKey == "B" {
			sb.Where(sb.In("p.brand_id", sqlbuilder.List(facetKeys)))
		} else if attributeSetID, err := strconv.Atoi(filterKey); err == nil {
			joinTable := fmt.Sprintf("product_attribute as pa_%d", attributeSetID)
			joinExpr1 := fmt.Sprintf("pa_%d.product_id = p.id", attributeSetID)
			joinExpr2 := sb.In(fmt.Sprintf("pa_%d.attribute_value_id", attributeSetID), sqlbuilder.List(facetKeys))
			sb.JoinWithOption(sqlbuilder.InnerJoin, joinTable, sb.And(joinExpr1, joinExpr2))
		}
	}

	return sb.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) FindAvailableByCategoryID(ctx context.Context, categoryID int64, filters model.FilterCombination) ([]model.Product, error) {
	const op = "sqlitedb.Product.FindAvailableByCategoryID"

	query, args := st.getQueryFindAvailableByCategoryID(categoryID, filters)
	rows, err := runSelect[model.Product](ctx, st.db, query, args)
	if err != nil {
		return nil, utils.WrapIfErr(op, err)
	}

	for i := range rows {
		if err = st.fillDtoDetails(ctx, &rows[i]); err != nil {
			return rows, utils.WrapIfErr(op, err)
		}
	}

	return rows, nil
}

func (st *Product) getQueryGetByID(id int64) (string, []any) {
	sb := st.getQuerySelectBase()
	sb.Where(sb.EQ("p.id", id))
	return sb.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) GetByID(ctx context.Context, id int64) (model.Product, bool, error) {
	const op = "sqlitedb.Product.GetByID"

	query, args := st.getQueryGetByID(id)
	row, ok, err := runGet[model.Product](ctx, st.db, query, args)
	if err != nil {
		return row, false, utils.WrapIfErr(op, err)
	} else if !ok {
		return row, false, nil
	}

	return row, true, nil
}

func (st *Product) fillDtoDetails(ctx context.Context, dto *model.Product) error {
	const op = "sqlitedb.Product.fillDtoDetails"
	attributes, _, err := st.FindManyAttributesByProductID(ctx, dto.ID, storage.FindManyParams{})
	dto.Attributes = attributes
	return utils.WrapIfErr(op, err)
}

func (st *Product) getQueryInsert(dto model.Product) (string, []any) {
	ib := sqlbuilder.InsertInto("product")
	ib.Cols(
		"code",
		"slug",
		"name",
		"description",
		"quantity",
		"price",
		"brand_id",
		"category_id",
		"supplier_id",
		"status_id",
		"is_published",
	)
	ib.Values(
		dto.Code,
		dto.Slugify(),
		dto.Name,
		dto.Description,
		dto.Quantity,
		dto.Price,
		dto.BrandID,
		dto.CategoryID,
		dto.SupplierID,
		dto.StatusID,
		dto.IsPublished,
	)
	return ib.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) getQueryUpdateByID(dto model.Product) (string, []any) {
	ub := sqlbuilder.Update("product")
	ub.Where(ub.EQ("id", dto.ID))
	ub.SetMore("updated_at = unixepoch()")
	slug := dto.Slugify()
	if slug != "" {
		ub.SetMore(ub.EQ("slug", slug))
	}
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "Code", "code", dto.Code)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "Name", "name", dto.Name)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "Description", "description", dto.Description)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "Quantity", "quantity", dto.Quantity)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "Price", "price", dto.Price)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "ProductBrandEmbed.BrandID", "brand_id", dto.BrandID)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "ProductCategoryEmbed.CategoryID", "category_id", dto.CategoryID)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "ProductSupplierEmbed.SupplierID", "supplier_id", dto.SupplierID)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "ProductStatusEmbed.StatusID", "status_id", dto.StatusID)
	storage.ApplyUpdateSetPartial(ub, dto.Partial, "IsPublished", "is_published", dto.IsPublished)
	return ub.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) getQueryDeleteInvalidAttributes(productID int64) (string, []any) {
	sb := sqlbuilder.Select("ats.id")
	sb.From("attribute_set as ats")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "attribute_group as atg", "atg.id = ats.attribute_group_id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category as c", "c.attribute_group_id = atg.id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "product as p", "p.category_id = c.id")
	sb.Where(sb.EQ("p.id", productID))

	dlb := sqlbuilder.DeleteFrom("product_attribute")
	dlb.Where(dlb.EQ("product_id", productID))
	dlb.Where(dlb.NotIn("attribute_set_id", sb))
	return dlb.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) UpsertOne(ctx context.Context, dto model.Product) (int64, error) {
	const op = "sqlitedb.Product.UpsertOne"

	tx, err := st.db.Beginx()
	if err != nil {
		return 0, utils.WrapIfErr(op, err)
	}
	defer tx.Rollback()

	if dto.ID == 0 {
		query, args := st.getQueryInsert(dto)
		if dto.ID, err = runExecInsert(ctx, tx, query, args); err != nil {
			return 0, utils.WrapIfErr(op, err)
		}
	} else {
		query, args := st.getQueryUpdateByID(dto)
		if _, err = runExecAffected(ctx, tx, query, args); err != nil {
			return 0, utils.WrapIfErr(op, err)
		}
	}

	query, args := st.getQueryDeleteInvalidAttributes(dto.ID)
	if _, err = runExecAffected(ctx, tx, query, args); err != nil {
		return 0, utils.WrapIfErr(op, err)
	}

	return dto.ID, utils.WrapIfErr(op, tx.Commit())
}

func (st *Product) UpdateMany(ctx context.Context, dtos []model.Product) (int64, error) {
	const op = "sqlitedb.Product.UpdateMany"

	queries, args := make([]string, len(dtos)), make([][]any, len(dtos))
	for i, dto := range dtos {
		queries[i], args[i] = st.getQueryUpdateByID(dto)
	}

	affected, err := runExecAffectedRangeNewTx(ctx, st.db, queries, args)
	if err != nil {
		return 0, utils.WrapIfErr(op, err)
	}

	return affected, utils.WrapIfErr(op, err)
}

func (st *Product) getQueryDeleteManyByID(ids []int64) (string, []any) {
	dlb := sqlbuilder.DeleteFrom("product")
	dlb.Where(dlb.In("id", sqlbuilder.List(ids)))
	return dlb.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) DeleteManyByID(ctx context.Context, ids []int64) (int64, error) {
	const op = "sqlitedb.Product.DeleteManyByID"
	query, args := st.getQueryDeleteManyByID(ids)
	affected, err := runExecAffected(ctx, st.db, query, args)
	return affected, utils.WrapIfErr(op, err)
}

func (st *Product) getQueryFindManyAttributesByProductID(productID int64, q storage.FindManyParams) (string, []any) {
	sb := sqlbuilder.Select(
		"ats.id as id",
		"ats.name",
		"ats.in_box",
		"ats.in_filter",
		"atv.id as value_id",
		"atv.name as value",
		"count(*) over () as count",
	)
	sb.From("product as p")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "category as c", "c.id = p.category_id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "attribute_group as atg", "atg.id = c.attribute_group_id")
	sb.JoinWithOption(sqlbuilder.InnerJoin, "attribute_set as ats", "ats.attribute_group_id = atg.id")
	sb.JoinWithOption(sqlbuilder.LeftJoin, "product_attribute as pa", "pa.product_id = p.id and pa.attribute_set_id = ats.id")
	sb.JoinWithOption(sqlbuilder.LeftJoin, "attribute_value as atv", "atv.id = pa.attribute_value_id")
	sb.Where(sb.EQ("p.id", productID))
	sb.OrderBy("ats.position", "ats.id DESC")
	storage.ApplyLimitOffset(sb, q.Limit, q.Offset)
	return sb.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) FindManyAttributesByProductID(ctx context.Context, productID int64, q storage.FindManyParams) ([]model.ProductAttribute, int64, error) {
	const op = "sqlitedb.Product.FindManyAttributesByProductID"

	type LocalResult struct {
		model.ProductAttribute
		Count int64 `db:"count"`
	}

	query, args := st.getQueryFindManyAttributesByProductID(productID, q)
	rows, err := runSelect[LocalResult](ctx, st.db, query, args)
	if err != nil {
		return nil, 0, utils.WrapIfErr(op, err)
	}

	dto, count := make([]model.ProductAttribute, len(rows)), int64(0)
	for i, row := range rows {
		dto[i], count = row.ProductAttribute, row.Count
	}

	return dto, count, nil
}

func (st *Product) getQueryUpsertAttribute(productID int64, dto model.ProductAttribute) (string, []any) {
	ib := sqlbuilder.ReplaceInto("product_attribute")
	ib.Cols("product_id", "attribute_set_id", "attribute_value_id")
	ib.Values(productID, dto.AttributeSetID, dto.AttributeValueID)
	return ib.BuildWithFlavor(sqlbuilder.SQLite)
}

func (st *Product) UpsertManyAttributes(ctx context.Context, productID int64, dtos []model.ProductAttribute) (int64, error) {
	const op = "sqlitedb.Product.UpsertManyAttributes"

	queries, args := make([]string, len(dtos)), make([][]any, len(dtos))
	for i, dto := range dtos {
		queries[i], args[i] = st.getQueryUpsertAttribute(productID, dto)
	}

	tx, err := st.db.Beginx()
	if err != nil {
		return 0, utils.WrapIfErr(op, err)
	}
	defer tx.Rollback()

	affected, err := runExecAffectedRangeTx(ctx, tx, queries, args)
	if err != nil {
		return 0, utils.WrapIfErr(op, err)
	}

	return affected, utils.WrapIfErr(op, tx.Commit())
}
