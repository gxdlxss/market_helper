package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type Config struct {
	BaseDir  string   `json:"base_dir"`
	Selected []string `json:"selected"`
}

type Sale struct {
	Time      time.Time
	Server    string
	Character string
	Item      string
	Quantity  int
	Price     float64
}

type ItemStats struct {
	Count int
	Sum   float64
}

type Character struct {
	ID       string
	Name     string
	LastSeen time.Time
	Items    map[string]*ItemStats
}

type Server struct {
	Name       string
	Characters map[string]*Character
}

var (
	exportRe = regexp.MustCompile(`^ChatExport_(\d{4}-\d{2}-\d{2})(?: \((\d+)\))?$`)
	saleRe   = regexp.MustCompile(`(?s)Сервер:\s*(.+?)\s*Персонаж:\s*(.+?)\s*(?:Название|Предмет):\s*(.+?)\s*(?:Кол-во|Количество):\s*([0-9]+)\s*Цена продажи:\s*\$([0-9\s,]+)`) // nolint:lll
)

func main() {
	cfg, err := loadOrCreateConfig("config.json")
	if err != nil {
		log.Fatal(err)
	}

	dir, err := findLatestExport(cfg.BaseDir)
	if err != nil {
		log.Fatal(err)
	}

	filePath := filepath.Join(dir, "messages.html")
	f, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("не удалось открыть %s: %v", filePath, err)
	}
	defer f.Close()

	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		log.Fatalf("ошибка разбора HTML: %v", err)
	}

	var sales []Sale
	doc.Find("div.message").Each(func(_ int, msg *goquery.Selection) {
		text := msg.Find("div.text").Text()
		if !strings.Contains(text, "Вы успешно продали предмет") {
			return
		}

		dateTitle, ok := msg.Find("div.pull_right.date.details").Attr("title")
		if !ok {
			return
		}
		ts := strings.Split(dateTitle, " UTC")[0]
		msgTime, err := time.ParseInLocation("02.01.2006 15:04:05", ts, time.Local)
		if err != nil {
			return
		}

		m := saleRe.FindStringSubmatch(text)
		if len(m) != 6 {
			return
		}

		server := strings.TrimSpace(m[1])
		character := strings.TrimSpace(m[2])
		item := strings.TrimSpace(m[3])
		if item == "Улучшенный эпинефрин" {
			item = "Адреналин"
		}
		qty, _ := strconv.Atoi(m[4])
		priceStr := strings.ReplaceAll(strings.ReplaceAll(m[5], " ", ""), ",", ".")
		price, _ := strconv.ParseFloat(priceStr, 64)

		sales = append(sales, Sale{Time: msgTime, Server: server, Character: character, Item: item, Quantity: qty, Price: price})
	})

	now := time.Now()
	periods := []struct {
		name   string
		window time.Duration
	}{{"all", 0}, {"day", 24 * time.Hour}, {"week", 7 * 24 * time.Hour}, {"month", 30 * 24 * time.Hour}}

	aggByPeriod := make(map[string]map[string]*Server)
	for _, p := range periods {
		aggByPeriod[p.name] = aggregateSales(sales, now, p.window)
	}

	for _, srvName := range sortedServerKeys(aggByPeriod["all"]) {
		fmt.Printf("\nСервер: %s\n", srvName)
		for _, charID := range sortedCharIDs(aggByPeriod["all"][srvName]) {
			chAll := aggByPeriod["all"][srvName].Characters[charID]
			fmt.Printf("Персонаж %s #%s:\n", chAll.Name, chAll.ID)
			for _, p := range periods {
				fmt.Printf("  -- %s --\n", p.name)
				srv := aggByPeriod[p.name][srvName]
				if srv == nil {
					fmt.Println("    (нет данных)")
					continue
				}
				ch := srv.Characters[charID]
				if ch == nil {
					fmt.Println("    (нет данных)")
					continue
				}
				printCharacterItemStats(ch, cfg.Selected)
			}
		}
	}

	itemsSet := make(map[string]struct{})
	for _, s := range sales {
		itemsSet[s.Item] = struct{}{}
	}
	fmt.Println("\nСписок всех проданных предметов:")
	var allItems []string
	for it := range itemsSet {
		allItems = append(allItems, it)
	}
	sort.Strings(allItems)
	for _, it := range allItems {
		fmt.Println(" -", it)
	}

	fmt.Print("\nНажмите Enter для выхода...")
	bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func printCharacterItemStats(ch *Character, selected []string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Тип предмета\tКол-во\tСумма продаж\tСредняя цена")
	for _, item := range selected {
		d := ch.Items[item]
		if d == nil {
			continue
		}
		avg := 0.0
		if d.Count > 0 {
			avg = d.Sum / float64(d.Count)
		}
		fmt.Fprintf(w, "%s\t%d\t$%.2f\t$%.2f\n", item, d.Count, d.Sum, avg)
	}
	w.Flush()

	var sumSel, sumAll float64
	for _, item := range selected {
		if d := ch.Items[item]; d != nil {
			sumSel += d.Sum
		}
	}
	for _, d := range ch.Items {
		sumAll += d.Sum
	}
	fmt.Printf("    Сумма продаж выбранных позиций: $%.2f\n", sumSel)
	fmt.Printf("    Общая сумма продаж:             $%.2f\n", sumAll)
}

func aggregateSales(sales []Sale, now time.Time, window time.Duration) map[string]*Server {
	servers := make(map[string]*Server)
	for _, s := range sales {
		if window > 0 && now.Sub(s.Time) > window {
			continue
		}

		namePart, idPart := splitCharacter(s.Character)
		if idPart == "" {
			idPart = namePart
		}

		srv := servers[s.Server]
		if srv == nil {
			srv = &Server{Name: s.Server, Characters: make(map[string]*Character)}
			servers[s.Server] = srv
		}

		ch := srv.Characters[idPart]
		if ch == nil {
			ch = &Character{ID: idPart, Name: namePart, LastSeen: s.Time, Items: make(map[string]*ItemStats)}
			srv.Characters[idPart] = ch
		} else if s.Time.After(ch.LastSeen) {
			ch.Name = namePart
			ch.LastSeen = s.Time
		}

		stats := ch.Items[s.Item]
		if stats == nil {
			stats = &ItemStats{}
			ch.Items[s.Item] = stats
		}
		stats.Count += s.Quantity
		stats.Sum += s.Price
	}
	return servers
}

func sortedServerKeys(m map[string]*Server) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedCharIDs(srv *Server) []string {
	keys := make([]string, 0, len(srv.Characters))
	for id := range srv.Characters {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		return srv.Characters[keys[i]].Name < srv.Characters[keys[j]].Name
	})
	return keys
}

func splitCharacter(full string) (name, id string) {
	if i := strings.LastIndex(full, "#"); i != -1 {
		name = strings.TrimSpace(full[:i])
		id = strings.TrimSpace(full[i+1:])
	} else {
		name = strings.TrimSpace(full)
	}
	return
}

func loadOrCreateConfig(path string) (*Config, error) {
	var cfg Config
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		if err := json.NewDecoder(file).Decode(&cfg); err == nil && cfg.BaseDir != "" && len(cfg.Selected) > 0 {
			return &cfg, nil
		}
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Введите путь к каталогу ChatExport_*: ")
	baseDir, _ := reader.ReadString('\n')
	baseDir = strings.TrimSpace(baseDir)

	fmt.Println("Введите названия предметов (пустая строка для завершения):")
	var items []string
	for {
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		items = append(items, line)
	}

	cfg = Config{BaseDir: baseDir, Selected: items}
	f, err := os.Create(path)
	if err == nil {
		defer f.Close()
		_ = json.NewEncoder(f).Encode(cfg)
	}
	return &cfg, nil
}

func findLatestExport(base string) (string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("не удалось открыть %s: %w", base, err)
	}
	var best *struct {
		path    string
		date    time.Time
		variant int
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := exportRe.FindStringSubmatch(e.Name())
		if len(m) == 0 {
			continue
		}
		d, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		v := 0
		if m[2] != "" {
			v, _ = strconv.Atoi(m[2])
		}
		info := &struct {
			path    string
			date    time.Time
			variant int
		}{filepath.Join(base, e.Name()), d, v}
		if best == nil || info.date.After(best.date) || (info.date.Equal(best.date) && info.variant > best.variant) {
			best = info
		}
	}
	if best == nil {
		return "", fmt.Errorf("не найдено ни одной папки ChatExport_* в %s", base)
	}
	return best.path, nil
}
