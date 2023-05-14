package handlers

import (
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chazari-x/yandex-pr-diplom/internal/app/config"
	"github.com/chazari-x/yandex-pr-diplom/internal/app/database"
)

type Controller struct {
	c  config.Config
	db *database.DataBase
}

func NewController(c config.Config, db *database.DataBase) *Controller {
	return &Controller{c: c, db: db}
}

type Middleware func(http.Handler) http.Handler

func MiddlewaresConveyor(h http.Handler) http.Handler {
	middlewares := []Middleware{gzipMiddleware, cookieMiddleware}
	for _, middleware := range middlewares {
		h = middleware(h)
	}
	return h
}

type gzipWriter struct {
	http.ResponseWriter
	Writer io.Writer
}

func (w gzipWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				log.Print("gzipMiddleware: new reader err: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			defer func() {
				_ = gz.Close()
			}()

			r.Body = gz
		}

		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			log.Print("gzipMiddleware: new writer level err: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		defer func() {
			_ = gz.Close()
		}()

		w.Header().Set("Content-Encoding", "gzip")
		next.ServeHTTP(gzipWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

func generateRandom(size int) ([]byte, error) {
	b := make([]byte, size)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func makeUserIdentification() (string, error) {
	str := time.Now().Format("02012006150405")

	key, err := generateRandom(aes.BlockSize)
	if err != nil {
		return "", err
	}

	aesblock, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesgcm, err := cipher.NewGCM(aesblock)
	if err != nil {
		return "", err
	}

	nonce, err := generateRandom(aesgcm.NonceSize())
	if err != nil {
		return "", err
	}

	id := fmt.Sprintf("%x", aesgcm.Seal(nil, nonce, []byte(str), nil))

	return id, nil
}

var userIdentification = "user_identification"

var identification struct {
	ID string
}

func cookieMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var uid string

		cookie, err := r.Cookie(userIdentification)
		if err != nil {
			if !errors.Is(err, http.ErrNoCookie) {
				log.Print("cookieMiddleware: r.Cookie err: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			uid, err = setCookie(w)
			if err != nil {
				log.Print("cookieMiddleware: set user identification err: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		} else {
			uid = cookie.Value
		}

		ctx := context.WithValue(r.Context(), identification, uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func setCookie(w http.ResponseWriter) (string, error) {
	uid, err := makeUserIdentification()
	if err != nil {
		return "", err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     userIdentification,
		Value:    uid,
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: false,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
	})

	return uid, nil
}

type userStruct struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (c *Controller) PostRegister(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	cookie := fmt.Sprintf("%v", r.Context().Value(identification))

	b, err := io.ReadAll(r.Body)
	if err != nil {
		log.Print("PostRegister: read all err: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if string(b) == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	user := userStruct{}

	err = json.Unmarshal(b, &user)
	if err != nil {
		log.Print("PostRegister: json unmarshal err: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var status = http.StatusOK

	for i := 0; i < 2; i++ {
		err = c.db.Register(user.Login, user.Password, cookie)
		if err == nil {
			break
		}

		if errors.Is(err, c.db.Err.RegisterConflict) {
			status = http.StatusConflict
			break
		}

		if !errors.Is(err, c.db.Err.Duplicate) {
			log.Printf("register: %s, login: %s, password: %s", err, user.Login, user.Password)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		cookie, err = setCookie(w)
		if err != nil {
			log.Print("PostRegister: set cookie err: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	log.Printf("register: %d, cookie: %s, login: %s, password: %s", status, cookie, user.Login, user.Password)
	w.WriteHeader(status)
}

func (c *Controller) PostLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	cookie := fmt.Sprintf("%v", r.Context().Value(identification))

	b, err := io.ReadAll(r.Body)
	if err != nil {
		log.Print("PostLogin: read all err: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if string(b) == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	user := userStruct{}

	err = json.Unmarshal(b, &user)
	if err != nil {
		log.Print("PostLogin: json unmarshal err: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var status = http.StatusOK

	err = c.db.Login(user.Login, user.Password, cookie)
	if err != nil {
		if !errors.Is(err, c.db.Err.Empty) {
			log.Printf("login: %s, login: %s, password: %s", err, user.Login, user.Password)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		status = http.StatusUnauthorized
	}

	log.Printf("login: %d, cookie: %s, login: %s, password: %s", status, cookie, user.Login, user.Password)
	w.WriteHeader(status)
}

func (c *Controller) PostOrders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	cookie := fmt.Sprintf("%v", r.Context().Value(identification))

	b, err := io.ReadAll(r.Body)
	if err != nil {
		log.Print("PostOrders: read all err: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if string(b) == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var order int

	err = json.Unmarshal(b, &order)
	if err != nil {
		log.Print("PostOrders: json unmarshal err: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var status = http.StatusAccepted

	err = c.db.AddOrder(cookie, order)
	if err != nil {
		if errors.Is(err, c.db.Err.NoAuthorization) {
			status = http.StatusUnauthorized
		} else if errors.Is(err, c.db.Err.Duplicate) {
			status = http.StatusOK
		} else if errors.Is(err, c.db.Err.Used) {
			status = http.StatusConflict
		} else {
			log.Print("PostOrders: add order err: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	log.Printf("orders: %d, cookie: %s, order: %d", status, cookie, order)
	w.WriteHeader(status)
}