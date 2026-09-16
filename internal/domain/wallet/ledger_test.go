package wallet_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

func TestNewLedgerEntryEnforcesTheBalanceEquation(t *testing.T) {
	tests := map[string]struct {
		direction                           wallet.Direction
		amount, balanceBefore, balanceAfter string
		wantErr                             bool
	}{
		"debit holds":         {wallet.DirectionDebit, "25.00", "1000.00", "975.00", false},
		"credit holds":        {wallet.DirectionCredit, "25.00", "1000.00", "1025.00", false},
		"debit off by a cent": {wallet.DirectionDebit, "25.00", "1000.00", "975.01", true},
		"debit signed wrong":  {wallet.DirectionDebit, "25.00", "1000.00", "1025.00", true},
		"credit signed wrong": {wallet.DirectionCredit, "25.00", "1000.00", "975.00", true},
		"after untouched":     {wallet.DirectionDebit, "25.00", "1000.00", "1000.00", true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			entry, err := wallet.NewLedgerEntry(entryID, walletID, txID, test.direction,
				brl(t, test.amount), brl(t, test.balanceBefore), brl(t, test.balanceAfter), movedAt)

			switch {
			case test.wantErr && !errors.Is(err, wallet.ErrLedgerEquation):
				t.Fatalf("error = %v, want ErrLedgerEquation", err)
			case !test.wantErr && err != nil:
				t.Fatalf("error = %v, want nil", err)
			case !test.wantErr && entry.Amount().Minor() != brl(t, test.amount).Minor():
				t.Errorf("Amount() = %s, want %s", entry.Amount(), test.amount)
			}
		})
	}
}

func TestNewLedgerEntryRejectsInvalidInput(t *testing.T) {
	before, after := brl(t, "1000.00"), brl(t, "975.00")
	amount := brl(t, "25.00")
	euro, err := money.Parse("25.00", money.EUR)
	if err != nil {
		t.Fatalf("Parse error = %v", err)
	}
	negative, err := money.FromMinor(-2500, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}
	zero, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatalf("Zero error = %v", err)
	}

	tests := map[string]struct {
		id, wallet, tx uuid.UUID
		direction      wallet.Direction
		amount         money.Money
		createdAt      time.Time
		want           error
	}{
		"nil id":            {uuid.Nil(), walletID, txID, wallet.DirectionDebit, amount, movedAt, wallet.ErrUninitialized},
		"nil wallet":        {entryID, uuid.Nil(), txID, wallet.DirectionDebit, amount, movedAt, wallet.ErrUninitialized},
		"nil transaction":   {entryID, walletID, uuid.Nil(), wallet.DirectionDebit, amount, movedAt, wallet.ErrUninitialized},
		"empty direction":   {entryID, walletID, txID, "", amount, movedAt, wallet.ErrInvalidDirection},
		"unknown direction": {entryID, walletID, txID, wallet.Direction("TRANSFER"), amount, movedAt, wallet.ErrInvalidDirection},
		"invalid money":     {entryID, walletID, txID, wallet.DirectionDebit, money.Money{}, movedAt, wallet.ErrUninitialized},
		"negative amount":   {entryID, walletID, txID, wallet.DirectionDebit, negative, movedAt, money.ErrNegativeAmount},
		"zero amount":       {entryID, walletID, txID, wallet.DirectionDebit, zero, movedAt, wallet.ErrEmptyMovement},
		"currency mismatch": {entryID, walletID, txID, wallet.DirectionDebit, euro, movedAt, money.ErrCurrencyMismatch},
		"zero time":         {entryID, walletID, txID, wallet.DirectionDebit, amount, time.Time{}, wallet.ErrUninitialized},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := wallet.NewLedgerEntry(test.id, test.wallet, test.tx, test.direction, test.amount, before, after, test.createdAt)
			if !errors.Is(err, test.want) {
				t.Errorf("NewLedgerEntry() error = %v, want %v", err, test.want)
			}
		})
	}
}

// §6.4's immutability is checked rather than reviewed: a reader is one keystroke
// from being a setter, and nothing else would catch it.
func TestLedgerEntryHasNoMutatingMethods(t *testing.T) {
	files, err := parser.ParseDir(token.NewFileSet(), ".", nil, 0)
	if err != nil {
		t.Fatalf("ParseDir() error = %v", err)
	}

	for _, pkg := range files {
		ast.Inspect(pkg, func(node ast.Node) bool {
			decl, ok := node.(*ast.FuncDecl)
			if !ok || decl.Recv == nil || receiverType(decl) != "LedgerEntry" {
				return true
			}
			ast.Inspect(decl.Body, func(inner ast.Node) bool {
				switch statement := inner.(type) {
				case *ast.AssignStmt:
					for _, target := range statement.Lhs {
						if _, isField := target.(*ast.SelectorExpr); isField {
							t.Errorf("%s assigns to a receiver field: LedgerEntry must be immutable", decl.Name.Name)
						}
					}
				case *ast.UnaryExpr:
					if statement.Op == token.AND {
						if _, isField := statement.X.(*ast.SelectorExpr); isField {
							t.Errorf("%s takes the address of a receiver field: LedgerEntry must be immutable", decl.Name.Name)
						}
					}
				}
				return true
			})
			return true
		})
	}
}

func receiverType(decl *ast.FuncDecl) string {
	expression := decl.Recv.List[0].Type
	if star, ok := expression.(*ast.StarExpr); ok {
		expression = star.X
	}
	if name, ok := expression.(*ast.Ident); ok {
		return name.Name
	}
	return ""
}
