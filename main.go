package main

import (
        "flag"
        "fmt"
        "log"
        "net/http"
        "os"
        "strconv"
        "sync"
        "time"

        "github.com/gin-contrib/cors"
        "github.com/gin-gonic/gin"
        "github.com/glebarez/sqlite" // SQLite без CGO
        "github.com/streadway/amqp" // RabbitMQ
        "gopkg.in/yaml.v2"
        "gorm.io/driver/postgres"
        "gorm.io/datatypes"
        "gorm.io/gorm"
)

// Config описывает конфигурацию сервера, базы данных, админки и RabbitMQ.
type Config struct {
        Server struct {
                Port string `yaml:"port"`
                Cors struct {
                        AllowedOrigins []string `yaml:"allowedOrigins"`
                        AllowedMethods []string `yaml:"allowedMethods"`
                        AllowedHeaders []string `yaml:"allowedHeaders"`
                } `yaml:"cors"`
        } `yaml:"server"`
        Database struct {
                Type string `yaml:"type"`
                DSN  string `yaml:"dsn"`
        } `yaml:"database"`
        Admin struct {
                APIKey  string `yaml:"api_key"`
                APIHash string `yaml:"api_hash"`
                Cors    struct {
                        AllowedOrigins []string `yaml:"allowedOrigins"`
                        AllowedMethods []string `yaml:"allowedMethods"`
                        AllowedHeaders []string `yaml:"allowedHeaders"`
                } `yaml:"cors"`
        } `yaml:"admin"`
        RabbitMQ struct {
                URL   string `yaml:"url"`
                Queue string `yaml:"queue"`
        } `yaml:"rabbitmq"`
}

// LoadConfig загружает конфигурацию из YAML-файла.
func LoadConfig(filename string) (*Config, error) {
        data, err := os.ReadFile(filename)
        if err != nil {
                return nil, err
        }
        var conf Config
        if err := yaml.Unmarshal(data, &conf); err != nil {
                return nil, err
        }
        return &conf, nil
}

// ShopPackage описывает модель товара.
type ShopPackage struct {
        ID        uint           `gorm:"primaryKey" json:"id"`
        CreatedAt time.Time      `json:"createdAt"`
        UpdatedAt time.Time      `json:"updatedAt"`
        Name      string         `json:"name"`
        Category  string         `json:"category"`
        BasePrice int            `json:"basePrice"` // цена в рублях
        Coins     int            `json:"coins"`
        Bonus     int            `json:"bonus"`
        Image     string         `json:"image"`
        Features  datatypes.JSON `json:"features"` // JSON-массив строк
        // Новые поля:
        Commands  datatypes.JSON `json:"commands"`  // JSON-массив строк
        Items     datatypes.JSON `json:"items"`     // JSON-массив с произвольным JSON
        ServerID  uint           `json:"serverId"`  // Привязка к серверу
}

// Server описывает сервер с собственным списком товаров.
type Server struct {
        ID        uint           `gorm:"primaryKey" json:"id"`
        CreatedAt time.Time      `json:"createdAt"`
        UpdatedAt time.Time      `json:"updatedAt"`
        Name      string         `json:"name"`
        Products  []ShopPackage  `gorm:"foreignKey:ServerID" json:"products"`
}

var (
        db                *gorm.DB
        serversCache      []Server
        serversCacheMutex sync.RWMutex

        rabbitConn    *amqp.Connection
        rabbitChannel *amqp.Channel
)

// createDefaultConfig записывает в указанный файл пример конфигурации.
func createDefaultConfig(filename string) error {
        defaultConfig := `server:
  port: "8080"
  cors:
    allowedOrigins: ["*"]
    allowedMethods: ["GET"]
    allowedHeaders: ["Content-Type"]
database:
  type: "sqlite"
  dsn: "shop.db"
admin:
  api_key: "your_api_key"
  api_hash: "your_api_hash"
  cors:
    allowedOrigins: ["*"]
    allowedMethods: ["GET", "POST", "PUT", "DELETE"]
    allowedHeaders: ["Content-Type", "API-KEY", "API-HASH"]
rabbitmq:
  url: "amqp://guest:guest@localhost:5672/"
  queue: "shop_queue"
`
        return os.WriteFile(filename, []byte(defaultConfig), 0644)
}

// initDB инициализирует подключение к базе данных.
func initDB(conf *Config) {
        var dialector gorm.Dialector
        switch conf.Database.Type {
        case "postgres":
                dialector = postgres.Open(conf.Database.DSN)
        case "sqlite":
                dialector = sqlite.Open(conf.Database.DSN)
        default:
                log.Fatalf("Неподдерживаемый тип базы данных: %s", conf.Database.Type)
        }
        var err error
        db, err = gorm.Open(dialector, &gorm.Config{})
        if err != nil {
                log.Fatalf("Ошибка подключения к базе данных: %v", err)
        }
}

// initRabbitMQ устанавливает подключение к RabbitMQ.
func initRabbitMQ(conf *Config) {
        var err error
        rabbitConn, err = amqp.Dial(conf.RabbitMQ.URL)
        if err != nil {
                log.Fatalf("Ошибка подключения к RabbitMQ: %v", err)
        }
        rabbitChannel, err = rabbitConn.Channel()
        if err != nil {
                log.Fatalf("Ошибка открытия канала RabbitMQ: %v", err)
        }
        log.Println("RabbitMQ подключен!")
}

// runMigrate выполняет миграцию базы данных и выходит.
func runMigrate(configPath string) {
        conf, err := LoadConfig(configPath)
        if err != nil {
                log.Fatalf("Ошибка загрузки конфигурации: %v", err)
        }
        initDB(conf)
        // Мигрируем обе модели: ShopPackage и Server
        if err := db.AutoMigrate(&ShopPackage{}, &Server{}); err != nil {
                log.Fatalf("Ошибка миграции: %v", err)
        }
        log.Println("Миграция успешно выполнена!")
}

// runServe запускает микросервис.
func runServe(configPath string) {
        conf, err := LoadConfig(configPath)
        if err != nil {
                log.Fatalf("Ошибка загрузки конфигурации: %v", err)
        }
        initDB(conf)
        // Мигрируем обе модели
        if err := db.AutoMigrate(&ShopPackage{}, &Server{}); err != nil {
                log.Fatalf("Ошибка миграции: %v", err)
        }

        // Инициализируем RabbitMQ
        initRabbitMQ(conf)

        router := gin.Default()

        // Настройка публичного CORS
        publicCors := cors.Config{
                AllowOrigins:     conf.Server.Cors.AllowedOrigins,
                AllowMethods:     conf.Server.Cors.AllowedMethods,
                AllowHeaders:     conf.Server.Cors.AllowedHeaders,
                AllowCredentials: true,
                MaxAge:           12 * time.Hour,
        }
        router.Use(cors.New(publicCors))

        // Публичный эндпоинт для получения товаров по серверам (с in-memory кэшем)
        router.GET("/products", func(c *gin.Context) {
                serversCacheMutex.RLock()
                if serversCache != nil {
                        c.JSON(http.StatusOK, serversCache)
                        serversCacheMutex.RUnlock()
                        return
                }
                serversCacheMutex.RUnlock()

                var servers []Server
                // Загружаем серверы вместе с товарами
                if err := db.Preload("Products", func(db *gorm.DB) *gorm.DB {
                        return db.Order("created_at desc")
                }).Order("created_at desc").Find(&servers).Error; err != nil {
                        c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось получить товары"})
                        return
                }

                serversCacheMutex.Lock()
                serversCache = servers
                serversCacheMutex.Unlock()

                c.JSON(http.StatusOK, servers)
        })

        // Административная группа маршрутов
        adminCors := cors.Config{
                AllowOrigins:     conf.Admin.Cors.AllowedOrigins,
                AllowMethods:     conf.Admin.Cors.AllowedMethods,
                AllowHeaders:     conf.Admin.Cors.AllowedHeaders,
                AllowCredentials: true,
                MaxAge:           12 * time.Hour,
        }
        admin := router.Group("/admin/products")
        admin.Use(cors.New(adminCors))
        admin.Use(adminAuth(conf.Admin.APIKey, conf.Admin.APIHash))
        {
                admin.GET("", adminGetProducts)
                admin.GET("/:id", adminGetProduct)
                admin.POST("", adminCreateProduct)
                admin.PUT("/:id", adminUpdateProduct)
                admin.DELETE("/:id", adminDeleteProduct)
        }

        port := conf.Server.Port
        if port == "" {
                port = "11050"
        }
        log.Printf("Сервер запущен на порту %s", port)
        router.Run(":" + port)
}

// adminAuth проверяет заголовки API-KEY и API-HASH.
func adminAuth(expectedKey, expectedHash string) gin.HandlerFunc {
        return func(c *gin.Context) {
                apiKey := c.GetHeader("API-KEY")
                apiHash := c.GetHeader("API-HASH")
                if apiKey != expectedKey || apiHash != expectedHash {
                        c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
                        return
                }
                c.Next()
        }
}

// invalidateCache сбрасывает in-memory кэш.
func invalidateCache() {
        serversCacheMutex.Lock()
        serversCache = nil
        serversCacheMutex.Unlock()
}

// --------------------
// Административные обработчики
// --------------------

// adminGetProducts возвращает список товаров (без кэша).
func adminGetProducts(c *gin.Context) {
        var packages []ShopPackage
        if err := db.Order("created_at desc").Find(&packages).Error; err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось получить товары"})
                return
        }
        c.JSON(http.StatusOK, packages)
}

// adminGetProduct возвращает товар по ID.
func adminGetProduct(c *gin.Context) {
        idStr := c.Param("id")
        id, err := strconv.Atoi(idStr)
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "Некорректный идентификатор"})
                return
        }
        var pkg ShopPackage
        if err := db.First(&pkg, id).Error; err != nil {
                c.JSON(http.StatusNotFound, gin.H{"error": "Товар не найден"})
                return
        }
        c.JSON(http.StatusOK, pkg)
}

// adminCreateProduct создаёт новый товар и инвалидирует кэш.
func adminCreateProduct(c *gin.Context) {
        var input ShopPackage
        if err := c.ShouldBindJSON(&input); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат данных"})
                return
        }
        input.CreatedAt = time.Now()
        input.UpdatedAt = time.Now()
        if err := db.Create(&input).Error; err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось создать товар"})
                return
        }
        invalidateCache()
        c.JSON(http.StatusOK, input)
}

// adminUpdateProduct обновляет существующий товар и инвалидирует кэш.
func adminUpdateProduct(c *gin.Context) {
        idStr := c.Param("id")
        id, err := strconv.Atoi(idStr)
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "Некорректный идентификатор"})
                return
        }
        var pkg ShopPackage
        if err := db.First(&pkg, id).Error; err != nil {
                c.JSON(http.StatusNotFound, gin.H{"error": "Товар не найден"})
                return
        }
        var input ShopPackage
        if err := c.ShouldBindJSON(&input); err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "Неверный формат данных"})
                return
        }
        pkg.Name = input.Name
        pkg.Category = input.Category
        pkg.BasePrice = input.BasePrice
        pkg.Coins = input.Coins
        pkg.Bonus = input.Bonus
        pkg.Image = input.Image
        pkg.Features = input.Features
        pkg.Commands = input.Commands
        pkg.Items = input.Items
        pkg.ServerID = input.ServerID
        pkg.UpdatedAt = time.Now()
        if err := db.Save(&pkg).Error; err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось обновить товар"})
                return
        }
        invalidateCache()
        c.JSON(http.StatusOK, pkg)
}

// adminDeleteProduct удаляет товар по ID и инвалидирует кэш.
func adminDeleteProduct(c *gin.Context) {
        idStr := c.Param("id")
        id, err := strconv.Atoi(idStr)
        if err != nil {
                c.JSON(http.StatusBadRequest, gin.H{"error": "Некорректный идентификатор"})
                return
        }
        if err := db.Delete(&ShopPackage{}, id).Error; err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось удалить товар"})
                return
        }
        invalidateCache()
        c.JSON(http.StatusOK, gin.H{"message": "Товар удалён"})
}

// usage выводит справку по командам.
func usage() {
        fmt.Println("Использование:")
        fmt.Println("  <command> [options]")
        fmt.Println("Команды:")
        fmt.Println("  create-config   Создает файл конфигурации (по умолчанию config.yaml)")
        fmt.Println("  migrate         Выполняет миграцию базы данных и выходит")
        fmt.Println("  serve           Запускает микросервис")
}

func main() {
        if len(os.Args) < 2 {
                // Если команда не указана, по умолчанию запускаем сервер
                runServe("config.yaml")
                return
        }

        cmd := os.Args[1]
        switch cmd {
        case "create-config":
                fs := flag.NewFlagSet("create-config", flag.ExitOnError)
                configPath := fs.String("o", "config.yaml", "Путь к создаваемому файлу конфигурации")
                fs.Parse(os.Args[2:])
                if _, err := os.Stat(*configPath); err == nil {
                        log.Fatalf("Файл %s уже существует", *configPath)
                }
                if err := createDefaultConfig(*configPath); err != nil {
                        log.Fatalf("Ошибка создания конфигурации: %v", err)
                }
                log.Printf("Конфигурация успешно создана: %s", *configPath)
        case "migrate":
                fs := flag.NewFlagSet("migrate", flag.ExitOnError)
                configPath := fs.String("config", "config.yaml", "Путь к файлу конфигурации")
                fs.Parse(os.Args[2:])
                runMigrate(*configPath)
        case "serve":
                fs := flag.NewFlagSet("serve", flag.ExitOnError)
                configPath := fs.String("config", "config.yaml", "Путь к файлу конфигурации")
                fs.Parse(os.Args[2:])
                runServe(*configPath)
        default:
                usage()
        }
}