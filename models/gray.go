package models

import "errors"

type GrayRule struct {
	Include   bool   `json:"include"`    // 是否包含白名单
	Exclude   bool   `json:"exclude"`    // 是否排除白名单
	DB        string `json:"db"`         // 灰度规则的数据库名
	Table     string `json:"table"`      // 灰度规则的表名
	Column    string `json:"column"`     // 灰度规则的列名
	WhiteList []any  `json:"white_list"` // 白名单
}

func (g *GrayRule) verify() error {
	if g.DB == "" {
		return errors.New("db is required")
	}
	if g.Table == "" {
		return errors.New("table is required")
	}
	if g.Column == "" {
		return errors.New("column is required")
	}
	if len(g.WhiteList) == 0 {
		return errors.New("white_list is required")
	}
	if g.Include && g.Exclude {
		return errors.New("include and exclude cannot be both true")
	}
	if !g.Include && !g.Exclude {
		return errors.New("either include or exclude must be true")
	}

	return nil
}
