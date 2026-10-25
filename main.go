package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var embeddedWeb embed.FS

func main() {
	cfg := DefaultConfig()
	addr := flag.String("addr", ":8080", "адрес, на котором слушает сервер (:8080 — со всех интерфейсов, доступно с других устройств)")
	dataDir := flag.String("data", "", "папка с *.json вопросами (по умолчанию — вопросы, вшитые в программу)")
	publicFlag := flag.String("public-url", "", "адрес, по которому игра открывается из интернета (https://quiz.example.com): он подставляется в ссылки-приглашения")
	trustProxy := flag.Bool("trust-proxy", false, "брать адрес игрока из заголовков CF-Connecting-IP / X-Forwarded-For — только если игра доступна исключительно через туннель или прокси (тогда запускайте с -addr 127.0.0.1:8080)")
	tlsCert := flag.String("tls-cert", "", "файл сертификата: включает HTTPS (нужен и -tls-key)")
	tlsKey := flag.String("tls-key", "", "файл закрытого ключа к -tls-cert")
	skipSrc := flag.String("skip-src", "", "не загружать вопросы из этих источников, через запятую (chgk — вопросы ЧГК, их лицензия не разрешает публичный сервер)")
	maxPlayers := flag.Int("max-players", defaultMaxPlayer, "сколько игроков пускать на сервер одновременно (0 — без ограничения)")
	maxPerIP := flag.Int("max-per-ip", defaultMaxPerIP, "сколько открытых соединений разрешено с одного адреса (0 — без ограничения)")
	flag.IntVar(&cfg.Rounds, "rounds", cfg.Rounds, "раундов в матче")
	flag.IntVar(&cfg.QPerRound, "questions", cfg.QPerRound, "вопросов в раунде")
	secs := flag.Int("time", int(cfg.QuestionTime/time.Second), "секунд на ответ")
	flag.Parse()
	cfg.QuestionTime = time.Duration(*secs) * time.Second
	if cfg.Rounds < 1 || cfg.QPerRound < 1 || *secs < 3 {
		log.Fatal("rounds и questions должны быть >= 1, time >= 3")
	}

	var bank *Bank
	var err error
	if *dataDir != "" {
		bank, err = LoadBank(os.DirFS(*dataDir), ".", cfg.QPerRound)
	} else {
		bank, err = LoadBank(embeddedData, "data", cfg.QPerRound)
	}
	if err != nil {
		log.Fatalf("не удалось загрузить вопросы:\n%v", err)
	}
	if skip := strings.FieldsFunc(*skipSrc, func(r rune) bool { return r == ',' || r == ' ' }); len(skip) > 0 {
		bank = bank.Without(cfg.QPerRound, skip...)
	}
	if len(bank.Cats) < offeredCats {
		log.Fatalf("нужно минимум %d категорий, найдено %d", offeredCats, len(bank.Cats))
	}

	publicURL, err := normalizePublicURL(*publicFlag)
	if err != nil {
		log.Fatalf("некорректный -public-url: %v", err)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("для HTTPS нужны оба флага: -tls-cert и -tls-key")
	}
	if *maxPlayers < 0 || *maxPerIP < 0 {
		log.Fatal("-max-players и -max-per-ip не могут быть отрицательными")
	}
	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}

	web, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		log.Fatal(err)
	}

	_, port, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("некорректный -addr: %v", err)
	}
	urls := lanURLs(scheme, port)
	if loopbackAddr(*addr) {
		urls = nil // the port is closed to the network, the addresses would only mislead
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := NewHub(cfg, bank)
	hub.maxClients = *maxPlayers
	go hub.Janitor(ctx)
	srv := &Server{hub: hub, cfg: cfg, bank: bank, urls: urls, web: web, publicURL: publicURL, trustProxy: *trustProxy}
	srv.protect(*maxPerIP)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx }, // lets SSE streams end on shutdown
	}

	total := 0
	for _, c := range bank.Cats {
		total += len(c.Questions)
	}
	fmt.Printf("🧠 Борьба умов запущена — %d категорий, %d вопросов\n\n", len(bank.Cats), total)
	fmt.Printf("  На этом компьютере:  %s://localhost:%s\n", scheme, port)
	if loopbackAddr(*addr) {
		fmt.Printf("  Другие устройства не подключатся напрямую: -addr %s слушает только этот компьютер\n", *addr)
	} else if len(urls) == 0 {
		fmt.Println("  Адрес в локальной сети не найден — проверьте, что вы подключены к Wi-Fi")
	}
	for _, u := range urls {
		fmt.Printf("  С других устройств:  %s\n", u)
	}
	if publicURL != "" {
		fmt.Printf("  Из интернета:        %s\n", publicURL)
	} else {
		fmt.Println("  Из интернета:        см. README, раздел «Запуск в глобальной сети»")
	}
	if n := bank.CountFrom("chgk"); n > 0 && (publicURL != "" || *trustProxy || *tlsCert != "") {
		fmt.Printf("\n  ⚠ В игре есть вопросы ЧГК (%d) — их лицензия разрешает только личное использование.\n", n)
		fmt.Println("    Для открытого сервера запускайте с -skip-src chgk (см. data/NOTICE.md).")
	}
	if *trustProxy && !loopbackAddr(*addr) {
		fmt.Println("\n  ⚠ -trust-proxy при открытом -addr: любой может подделать свой адрес в заголовках.")
		fmt.Println("    Если игра доступна только через туннель, запускайте с -addr 127.0.0.1:" + port)
	}
	fmt.Println("\nCtrl+C — остановить сервер")

	errc := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			errc <- httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errc <- httpSrv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		log.Fatal(err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Print(err)
	}
}

// lanURLs lists the addresses under which other devices on the same network
// can reach this machine.
func lanURLs(scheme, port string) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var urls []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := ipn.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() {
				urls = append(urls, scheme+"://"+net.JoinHostPort(ip.String(), port))
			}
		}
	}
	if host, err := os.Hostname(); err == nil && strings.HasSuffix(host, ".local") {
		urls = append(urls, scheme+"://"+net.JoinHostPort(host, port))
	}
	return urls
}

// loopbackAddr reports whether a listen address accepts connections from this
// machine only.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
