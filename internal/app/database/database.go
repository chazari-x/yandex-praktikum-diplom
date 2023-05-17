package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chazari-x/yandex-pr-diplom/internal/app/config"
	_ "github.com/lib/pq"
)

type DataBase struct {
	ASA string
	DB  *sql.DB
	Err errs
}

type errs struct {
	RegisterConflict error
	Empty            error
	Duplicate        error
	NoAuthorization  error
	Used             error
	NoMoney          error
	WrongData        error
}

type User struct {
	UserID   string  `json:"user_id,omitempty"`
	Login    string  `json:"login,omitempty"`
	Password string  `json:"password,omitempty"`
	Cookie   string  `json:"cookie,omitempty"`
	Current  float64 `json:"current"`
	WithDraw float64 `json:"withdrawn"`
}

type Order struct {
	Number     string  `json:"number"`
	Login      string  `json:"login,omitempty"`
	Status     string  `json:"status"`
	Accrual    float64 `json:"accrual,omitempty"`
	UploadedAt string  `json:"uploaded_at,omitempty"`
}

type WithDraw struct {
	OrderID     string  `json:"order_id"`
	Login       string  `json:"login,omitempty"`
	Sum         float64 `json:"sum"`
	ProcessedAt string  `json:"processed_at"`
}

var (
	dbCreateTables = `CREATE TABLE IF NOT EXISTS users (
							userid			SERIAL  PRIMARY KEY NOT NULL,
							login			VARCHAR UNIQUE		NOT NULL,
							password		VARCHAR 			NOT NULL,
							cookie			VARCHAR UNIQUE		NULL,
							current			NUMERIC 			NOT NULL	DEFAULT 0,
							withdrawn		NUMERIC 			NOT NULL	DEFAULT 0);
	
					CREATE TABLE IF NOT EXISTS Orders (
							number 			VARCHAR PRIMARY KEY NOT NULL,
							login 			VARCHAR 			NOT NULL,
							status 			VARCHAR 			NOT NULL	DEFAULT 'NEW',
							accrual 		NUMERIC 			NULL,
							uploaded_at 	VARCHAR				NOT NULL);
	
					CREATE TABLE IF NOT EXISTS withdraw (
							orderID 		VARCHAR PRIMARY KEY NOT NULL,
							login 			VARCHAR 			NOT NULL,
							sum 			NUMERIC 			NOT NULL,
							processed_at	VARCHAR 			NOT NULL);`

	// Таблица пользователей users:
	dbRegistration  = `INSERT INTO users (login, password, cookie) VALUES ($1, $2, $3) ON CONFLICT(login) DO NOTHING`
	dbAuthorization = `SELECT cookie FROM users WHERE login = $1 AND password = $2`
	dbGetLogin      = `SELECT login FROM users WHERE cookie = $1`
	dbGetBalance    = `SELECT login, current, withdrawn FROM users WHERE cookie = $1`
	dbDellCookie    = `UPDATE users SET cookie = NULL WHERE cookie = $1`
	dbSetCookie     = `UPDATE users SET cookie = $1 WHERE login = $2 AND password = $3`
	dbSetBalance    = `UPDATE users SET current = $1, withdrawn = $2 WHERE cookie = $3`

	// Таблица заказов orders:
	dbAddOrder      = `INSERT INTO orders (number, login, uploaded_at) VALUES ($1, $2, $3) ON CONFLICT(number) DO NOTHING`
	dbGetOrders     = `SELECT number, status, accrual, uploaded_at FROM orders WHERE login = $1`
	dbGetOrderLogin = `SELECT login FROM orders WHERE number = $1`
	dbUpdateOrder   = `UPDATE orders SET status = $1, accrual = $2 WHERE number = $3`

	// Таблица операций withdraw:
	dbAddWithDraw = `INSERT INTO withdraw VALUES ($1, $2, $3, $4) ON CONFLICT(orderID) DO NOTHING`
	dbGetWithDraw = `SELECT orderID, sum, processed_at FROM withdraw WHERE login = $1`
)

func StartDB(c config.Config) (*DataBase, error) {
	db, err := sql.Open("postgres", c.DataBaseURI)
	if err != nil {
		return nil, fmt.Errorf("sql open err: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err = db.PingContext(ctx)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(dbCreateTables)
	if err != nil {
		return nil, err
	}

	var errs errs
	errs.Used = errors.New("used")
	errs.Empty = errors.New("empty")
	errs.Duplicate = errors.New("duplicate")
	errs.NoAuthorization = errors.New("no authorization")
	errs.RegisterConflict = errors.New("register conflict")
	errs.NoMoney = errors.New("no money")
	errs.WrongData = errors.New("wrong data")

	return &DataBase{ASA: c.AccrualSystemAddress, DB: db, Err: errs}, nil
}

func (db *DataBase) Register(login, pass, cookie string) error {
	exec, err := db.DB.Exec(dbRegistration, login, pass, cookie)
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate key value violates unique constraint \"users_cookie_key\"") {
			return err
		}

		_, err = db.DB.Exec(dbDellCookie, cookie)
		if err != nil {
			return err
		}

		_, err = db.DB.Exec(dbRegistration, login, pass, cookie)
		if err != nil {
			return err
		}

		return nil
	}

	affected, err := exec.RowsAffected()
	if err != nil {
		return err
	}

	if affected == 0 {
		return db.Err.RegisterConflict
	}

	return nil
}

func (db *DataBase) Login(login, pass, cookie string) error {
	var cookieDB string
	if err := db.DB.QueryRow(dbAuthorization, login, pass).Scan(&cookieDB); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Err.WrongData
		}

		if !strings.Contains(err.Error(), "name \"cookie\": converting NULL to string is unsupported") {
			return err
		}
	}

	if cookieDB != cookie {
		if _, err := db.DB.Exec(dbDellCookie, cookie); err != nil {
			return err
		}

		if _, err := db.DB.Exec(dbSetCookie, cookie, login, pass); err != nil {
			return err
		}
	}

	return nil
}

func (db *DataBase) AddOrder(cookie string, order int) error {
	var login string
	if err := db.DB.QueryRow(dbGetLogin, cookie).Scan(&login); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		return db.Err.NoAuthorization
	}

	exec, err := db.DB.Exec(dbAddOrder, order, login, time.Now().Format(time.RFC3339))
	if err != nil {
		return err
	}

	affected, err := exec.RowsAffected()
	if err != nil {
		return err
	}

	if affected == 0 {
		var orderLogin string
		if err = db.DB.QueryRow(dbGetOrderLogin, order).Scan(&orderLogin); err != nil {
			return err
		}

		if orderLogin != login {
			return db.Err.Used
		}

		return db.Err.Duplicate
	}

	db.getOrderInfo(strconv.Itoa(order))

	return nil
}

const workersCount = 1

var workers = 0

var inputCh = make(chan string)

func (db *DataBase) getOrderInfo(number string) {
	go func(number string) {
		inputCh <- number
	}(number)

	if workers < workersCount {
		for i := workers; i < workersCount; i++ {
			workers++
			db.newWorker(inputCh)
		}
	}
}

func (db *DataBase) newWorker(input chan string) {
	go func() {
		log.Print("starting goroutine")

		defer func() {
			db.newWorker(input)
			if x := recover(); x != nil {
				log.Print("run time panic: ", x)
			}
		}()

		for {
			for number := range input {
				req, err := http.NewRequest("GET", db.ASA+"/api/orders/"+number, nil)
				if err != nil {
					go func(number string) {
						inputCh <- number
					}(number)
					log.Printf("go number: %s, err: %s", number, err)
					return
				}

				ctx, cancel := context.WithTimeout(req.Context(), time.Second)
				req = req.WithContext(ctx)
				client := http.DefaultClient
				resp, err := client.Do(req)
				if err != nil {
					go func(number string) {
						inputCh <- number
					}(number)
					log.Printf("go number: %s, err: %s", number, err)
					resp.Body.Close()
					cancel()
					return
				}

				b, err := io.ReadAll(resp.Body)
				if err != nil {
					go func(number string) {
						inputCh <- number
					}(number)
					log.Printf("go number: %s, err: %s", number, err)
					resp.Body.Close()
					cancel()
					return
				}

				switch resp.Status {
				case "200":
					var order Order
					err = json.Unmarshal(b, &order)
					if err != nil {
						go func(number string) {
							inputCh <- number
						}(number)
						log.Printf("go number: %s, err: %s", number, err)
						resp.Body.Close()
						cancel()
						return
					}

					log.Printf("go number: %s, status: %s", number, order.Status)
					switch order.Status {
					case "PROCESSING":

						go func(number string) {
							inputCh <- number
						}(number)
						err := db.updateOrder(order)
						if err != nil {
							log.Printf("go number: %s, err: %s", number, err)
							resp.Body.Close()
							cancel()
							return
						}
					case "INVALID", "PROCESSED":
						err := db.updateOrder(order)
						if err != nil {
							go func(number string) {
								inputCh <- number
							}(number)
							log.Printf("go number: %s, err: %s", number, err)
							resp.Body.Close()
							cancel()
							return
						}
					default:
						go func(number string) {
							inputCh <- number
						}(number)
					}
				case "429":
					log.Printf("go number: %s, status: %s", number, resp.Status)
					go func(number string) {
						inputCh <- number
					}(number)
					atoi, err := strconv.Atoi(resp.Header.Get("Retry-After"))
					if err != nil {
						log.Printf("go number: %s, err: %s", number, err)
						time.Sleep(time.Second * 15)
					} else {
						time.Sleep(time.Second * time.Duration(atoi))
					}
				case "500":
					log.Printf("go number: %s, status: %s", number, resp.Status)
					go func(number string) {
						inputCh <- number
					}(number)
				case "204":
					log.Printf("go number: %s, status: %s", number, resp.Status)
					err := db.updateOrder(Order{Status: "INVALID", Number: number})
					if err != nil {
						go func(number string) {
							inputCh <- number
						}(number)
						log.Printf("go number: %s, err: %s", number, err)
						resp.Body.Close()
						cancel()
						return
					}
				default:
					log.Printf("go number: %s, status: %s", number, resp.Status)
				}

				resp.Body.Close()
				cancel()
			}

			time.Sleep(time.Second)
		}
	}()
}

func (db *DataBase) updateOrder(order Order) error {
	_, err := db.DB.Exec(dbUpdateOrder, order.Status, order.Accrual, order.Number)
	if err != nil {
		return err
	}

	return nil
}

func (db *DataBase) GetOrders(cookie string) ([]Order, error) {
	var login string
	if err := db.DB.QueryRow(dbGetLogin, cookie).Scan(&login); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		return nil, db.Err.NoAuthorization
	}

	rows, err := db.DB.Query(dbGetOrders, login)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	var orders []Order
	for rows.Next() {
		var order Order
		var accrual sql.NullFloat64
		if err = rows.Scan(&order.Number, &order.Status, &accrual, &order.UploadedAt); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}

			if accrual.Valid {
				order.Accrual = accrual.Float64
			}
		}

		orders = append(orders, order)
	}

	if rows.Err() != nil {
		return nil, err
	}

	if orders == nil {
		return nil, db.Err.Empty
	}

	return orders, nil
}

func (db *DataBase) GetBalance(cookie string) (User, error) {
	var balance User
	if err := db.DB.QueryRow(dbGetBalance, cookie).Scan(&balance.Login, &balance.Current, &balance.WithDraw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return User{}, err
		}

		return User{}, db.Err.NoAuthorization
	}

	return balance, nil
}

func (db *DataBase) AddWithDraw(cookie, order string, sum float64) error {
	var balance User
	if err := db.DB.QueryRow(dbGetBalance, cookie).Scan(&balance.Login, &balance.Current, &balance.WithDraw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		return db.Err.NoAuthorization
	}

	if balance.Current < sum {
		return db.Err.NoMoney
	}

	balance.Current -= sum
	balance.WithDraw += sum

	_, err := db.DB.Exec(dbAddWithDraw, order, balance.Login, sum, time.Now().Format(time.RFC3339))
	if err != nil {
		return err
	}

	_, err = db.DB.Exec(dbSetBalance, balance.Current, balance.WithDraw, cookie)
	if err != nil {
		return err
	}

	return nil
}

func (db *DataBase) GetWithDraw(cookie string) ([]WithDraw, error) {
	var login string
	if err := db.DB.QueryRow(dbGetLogin, cookie).Scan(&login); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		return nil, db.Err.NoAuthorization
	}

	rows, err := db.DB.Query(dbGetWithDraw, login)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	var withdraw []WithDraw
	for rows.Next() {
		var order WithDraw
		if err = rows.Scan(&order.OrderID, &order.Sum, &order.ProcessedAt); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}

		withdraw = append(withdraw, order)
	}

	if rows.Err() != nil {
		return nil, err
	}

	if withdraw == nil {
		return nil, db.Err.Empty
	}

	return withdraw, nil
}
