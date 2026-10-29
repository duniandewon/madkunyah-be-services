package catalog

import (
	"context"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/google/uuid"
)

type repository interface {
	ListCategories(ctx context.Context, includeArchived bool) ([]Category, error)
	GetCategory(ctx context.Context, id string) (*Category, error)
	CreateCategory(ctx context.Context, actorID string, c *Category) error
	UpdateCategory(ctx context.Context, actorID, id string, fn func(*Category) error) (*Category, error)

	ListProducts(ctx context.Context, f ProductFilter) ([]Product, error)
	GetProduct(ctx context.Context, id string) (*ProductDetail, error)
	CreateProduct(ctx context.Context, actorID string, p *Product) error
	UpdateProduct(ctx context.Context, actorID, id string, fn func(*Product) error) (*Product, error)
	SetProductGroups(ctx context.Context, actorID, productID string, groupIDs []string) ([]AttachedGroup, error)

	CreateVariant(ctx context.Context, actorID string, v *Variant) error
	UpdateVariant(ctx context.Context, actorID, productID, id string, fn func(*Variant) error) (*Variant, error)

	ListGroups(ctx context.Context, includeArchived bool) ([]ModifierGroup, error)
	GetGroup(ctx context.Context, id string) (*ModifierGroup, error)
	CreateGroup(ctx context.Context, actorID string, g *ModifierGroup) error
	UpdateGroup(ctx context.Context, actorID, id string, fn func(*ModifierGroup) error) (*ModifierGroup, error)

	CreateModifier(ctx context.Context, actorID string, m *Modifier) error
	UpdateModifier(ctx context.Context, actorID, groupID, id string, fn func(*Modifier) error) (*Modifier, error)
}

type Service struct {
	repo     repository
	priceMax int64
}

// NewService takes CATALOG_PRICE_MAX, the upper bound on every price field.
func NewService(repo repository, priceMax int64) *Service {
	return &Service{repo: repo, priceMax: priceMax}
}

var errCostForbidden = domain.NewError(domain.CodeForbidden, "Setting cost_price requires cost.view.")

func canViewCost(actor *domain.StaffAccess) bool {
	return actor != nil && actor.CanGlobal(domain.PermCostView)
}

func showCost(p *Product, actor *domain.StaffAccess) {
	p.CostPrice.Shown = canViewCost(actor)
}

func (s *Service) ListCategories(ctx context.Context, includeArchived bool) ([]Category, error) {
	return s.repo.ListCategories(ctx, includeArchived)
}

func (s *Service) GetCategory(ctx context.Context, id string) (*Category, error) {
	return s.repo.GetCategory(ctx, id)
}

func (s *Service) CreateCategory(ctx context.Context, actor *domain.StaffAccess, in *CategoryInput) (*Category, error) {
	c := checks{}
	cat := &Category{ID: uuid.NewString(), Name: c.name("name", in.Name, maxCategoryName),
		SortOrder: in.SortOrder, IsActive: true}
	if err := c.err(); err != nil {
		return nil, err
	}
	if err := s.repo.CreateCategory(ctx, actor.StaffID, cat); err != nil {
		return nil, err
	}
	return cat, nil
}

func (s *Service) UpdateCategory(ctx context.Context, actor *domain.StaffAccess, id string,
	in *CategoryPatch) (*Category, error) {
	return s.repo.UpdateCategory(ctx, actor.StaffID, id, func(cat *Category) error {
		c := checks{}
		if in.Name != nil {
			cat.Name = c.name("name", *in.Name, maxCategoryName)
		}
		setIf(&cat.SortOrder, in.SortOrder)
		setIf(&cat.IsActive, in.IsActive)
		return c.err()
	})
}

func (s *Service) ListProducts(ctx context.Context, actor *domain.StaffAccess, f ProductFilter) ([]Product, error) {
	products, err := s.repo.ListProducts(ctx, f)
	if err != nil {
		return nil, err
	}
	for i := range products {
		showCost(&products[i], actor)
	}
	return products, nil
}

func (s *Service) GetProduct(ctx context.Context, actor *domain.StaffAccess, id string) (*ProductDetail, error) {
	d, err := s.repo.GetProduct(ctx, id)
	if err != nil {
		return nil, err
	}
	showCost(&d.Product, actor)
	return d, nil
}

func (s *Service) CreateProduct(ctx context.Context, actor *domain.StaffAccess, in *ProductInput) (*Product, error) {
	if in.CostPrice != nil && !canViewCost(actor) {
		return nil, errCostForbidden
	}
	c := checks{}
	p := &Product{
		ID:           uuid.NewString(),
		CategoryID:   canonicalID(in.CategoryID),
		Name:         c.name("name", in.Name, maxProductName),
		Description:  c.text("description", in.Description, maxDescription),
		SKU:          c.text("sku", in.SKU, maxSKU),
		BasePrice:    *in.BasePrice,
		IsWebVisible: in.IsWebVisible == nil || *in.IsWebVisible,
		IsPOSVisible: in.IsPOSVisible == nil || *in.IsPOSVisible,
		SortOrder:    in.SortOrder,
		IsActive:     true,
	}
	p.CostPrice.Value = in.CostPrice
	s.checkProductPrices(c, p)
	p.AvailableFrom, p.AvailableTo = c.window(in.AvailableFrom, in.AvailableTo)
	if err := c.err(); err != nil {
		return nil, err
	}
	if err := s.repo.CreateProduct(ctx, actor.StaffID, p); err != nil {
		return nil, err
	}
	showCost(p, actor)
	return p, nil
}

func (s *Service) UpdateProduct(ctx context.Context, actor *domain.StaffAccess, id string,
	in *ProductPatch) (*Product, error) {
	if in.CostPrice.Set && !canViewCost(actor) {
		return nil, errCostForbidden
	}
	p, err := s.repo.UpdateProduct(ctx, actor.StaffID, id, func(p *Product) error {
		return s.applyProductPatch(p, in)
	})
	if err != nil {
		return nil, err
	}
	showCost(p, actor)
	return p, nil
}

func (s *Service) applyProductPatch(p *Product, in *ProductPatch) error {
	c := checks{}
	if in.CategoryID != nil {
		p.CategoryID = canonicalID(*in.CategoryID)
	}
	if in.Name != nil {
		p.Name = c.name("name", *in.Name, maxProductName)
	}
	if in.Description.Set && !c.badType("description", in.Description.Bad, "a string or null") {
		p.Description = c.text("description", in.Description.Ptr(), maxDescription)
	}
	if in.SKU.Set && !c.badType("sku", in.SKU.Bad, "a string or null") {
		p.SKU = c.text("sku", in.SKU.Ptr(), maxSKU)
	}
	setIf(&p.BasePrice, in.BasePrice)
	if in.CostPrice.Set && !c.badType("cost_price", in.CostPrice.Bad, "an integer or null") {
		p.CostPrice.Value = in.CostPrice.Ptr()
	}
	setIf(&p.IsWebVisible, in.IsWebVisible)
	setIf(&p.IsPOSVisible, in.IsPOSVisible)
	from, to := p.AvailableFrom, p.AvailableTo
	if in.AvailableFrom.Set && !c.badType("available_from", in.AvailableFrom.Bad, "a time or null") {
		from = in.AvailableFrom.Ptr()
	}
	if in.AvailableTo.Set && !c.badType("available_to", in.AvailableTo.Bad, "a time or null") {
		to = in.AvailableTo.Ptr()
	}
	p.AvailableFrom, p.AvailableTo = c.window(from, to)
	setIf(&p.SortOrder, in.SortOrder)
	setIf(&p.IsActive, in.IsActive)
	s.checkProductPrices(c, p)
	return c.err()
}

func (s *Service) checkProductPrices(c checks, p *Product) {
	c.price("base_price", p.BasePrice, s.priceMax)
	if p.CostPrice.Value != nil {
		c.price("cost_price", *p.CostPrice.Value, s.priceMax)
	}
}

// SetProductGroups replaces the product's attached groups; the request
// order becomes the display order.
func (s *Service) SetProductGroups(ctx context.Context, actor *domain.StaffAccess, productID string,
	in *AttachInput) ([]AttachedGroup, error) {
	ids := make([]string, 0, len(in.ModifierGroupIDs))
	seen := map[string]bool{}
	for _, raw := range in.ModifierGroupIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, domain.ValidationError(map[string]string{"modifier_group_ids": "must contain UUIDs"})
		}
		if seen[id.String()] {
			return nil, domain.ValidationError(map[string]string{"modifier_group_ids": "must not repeat a group"})
		}
		seen[id.String()] = true
		ids = append(ids, id.String())
	}
	return s.repo.SetProductGroups(ctx, actor.StaffID, productID, ids)
}

func (s *Service) CreateVariant(ctx context.Context, actor *domain.StaffAccess, productID string,
	in *VariantInput) (*Variant, error) {
	c := checks{}
	v := &Variant{
		ID:         uuid.NewString(),
		ProductID:  productID,
		Name:       c.name("name", in.Name, maxVariantName),
		PriceDelta: in.PriceDelta,
		SKU:        c.text("sku", in.SKU, maxSKU),
		SortOrder:  in.SortOrder,
		IsActive:   true,
	}
	c.price("price_delta", v.PriceDelta, s.priceMax)
	if err := c.err(); err != nil {
		return nil, err
	}
	if err := s.repo.CreateVariant(ctx, actor.StaffID, v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Service) UpdateVariant(ctx context.Context, actor *domain.StaffAccess, productID, id string,
	in *VariantPatch) (*Variant, error) {
	return s.repo.UpdateVariant(ctx, actor.StaffID, productID, id, func(v *Variant) error {
		c := checks{}
		if in.Name != nil {
			v.Name = c.name("name", *in.Name, maxVariantName)
		}
		setIf(&v.PriceDelta, in.PriceDelta)
		if in.SKU.Set && !c.badType("sku", in.SKU.Bad, "a string or null") {
			v.SKU = c.text("sku", in.SKU.Ptr(), maxSKU)
		}
		setIf(&v.SortOrder, in.SortOrder)
		setIf(&v.IsActive, in.IsActive)
		c.price("price_delta", v.PriceDelta, s.priceMax)
		return c.err()
	})
}

func (s *Service) ListGroups(ctx context.Context, includeArchived bool) ([]ModifierGroup, error) {
	return s.repo.ListGroups(ctx, includeArchived)
}

func (s *Service) GetGroup(ctx context.Context, id string) (*ModifierGroup, error) {
	return s.repo.GetGroup(ctx, id)
}

func (s *Service) CreateGroup(ctx context.Context, actor *domain.StaffAccess, in *GroupInput) (*ModifierGroup, error) {
	c := checks{}
	g := &ModifierGroup{
		ID:        uuid.NewString(),
		Name:      c.name("name", in.Name, maxGroupName),
		MinSelect: in.MinSelect,
		MaxSelect: 1,
		IsActive:  true,
		Modifiers: []Modifier{},
	}
	setIf(&g.MaxSelect, in.MaxSelect)
	c.selectRange(g.MinSelect, g.MaxSelect)
	g.Required = g.MinSelect >= 1
	if err := c.err(); err != nil {
		return nil, err
	}
	if err := s.repo.CreateGroup(ctx, actor.StaffID, g); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Service) UpdateGroup(ctx context.Context, actor *domain.StaffAccess, id string,
	in *GroupPatch) (*ModifierGroup, error) {
	return s.repo.UpdateGroup(ctx, actor.StaffID, id, func(g *ModifierGroup) error {
		c := checks{}
		if in.Name != nil {
			g.Name = c.name("name", *in.Name, maxGroupName)
		}
		setIf(&g.MinSelect, in.MinSelect)
		setIf(&g.MaxSelect, in.MaxSelect)
		setIf(&g.IsActive, in.IsActive)
		c.selectRange(g.MinSelect, g.MaxSelect)
		return c.err()
	})
}

func (s *Service) CreateModifier(ctx context.Context, actor *domain.StaffAccess, groupID string,
	in *ModifierInput) (*Modifier, error) {
	c := checks{}
	m := &Modifier{
		ID:              uuid.NewString(),
		ModifierGroupID: groupID,
		Name:            c.name("name", in.Name, maxModifierName),
		PriceDelta:      in.PriceDelta,
		SortOrder:       in.SortOrder,
		IsActive:        true,
	}
	c.price("price_delta", m.PriceDelta, s.priceMax)
	if err := c.err(); err != nil {
		return nil, err
	}
	if err := s.repo.CreateModifier(ctx, actor.StaffID, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) UpdateModifier(ctx context.Context, actor *domain.StaffAccess, groupID, id string,
	in *ModifierPatch) (*Modifier, error) {
	return s.repo.UpdateModifier(ctx, actor.StaffID, groupID, id, func(m *Modifier) error {
		c := checks{}
		if in.Name != nil {
			m.Name = c.name("name", *in.Name, maxModifierName)
		}
		setIf(&m.PriceDelta, in.PriceDelta)
		setIf(&m.SortOrder, in.SortOrder)
		setIf(&m.IsActive, in.IsActive)
		c.price("price_delta", m.PriceDelta, s.priceMax)
		return c.err()
	})
}

// canonicalID lowercases a UUID the handler has already validated, so a
// differently cased ID is not mistaken for a change.
func canonicalID(s string) string {
	if id, err := uuid.Parse(s); err == nil {
		return id.String()
	}
	return s
}

// setIf assigns a PATCH field that was present.
func setIf[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}
