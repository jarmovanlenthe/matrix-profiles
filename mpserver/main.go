package main

import (
	"encoding/gob"
	"encoding/json"
	"errors"
	"io/ioutil"
	"os"
	"strconv"

	"github.com/aouyang1/go-matrixprofile/matrixprofile"
	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/redis"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	mpConcurrency    = 2
	maxRedisBlobSize = 1024 * 1024
	retentionPeriod  = 5 * 60
	redisURL         = "localhost:6379" // override with REDIS_URL environment variable
	port             = "8081"           // override with PORT environment variable

	requestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mpserver_requests_total",
			Help: "count of all HTTP requests for the mpserver",
		},
		[]string{"method", "endpoint", "code"},
	)
)

type RespError struct {
	Error        error `json:"error"`
	CacheExpired bool  `json:"cache_expired"`
}

func init() {
	prometheus.MustRegister(requestTotal)
}

func main() {
	r := gin.Default()

	store, err := initRedis()
	if err != nil {
		panic(err)
	}

	r.Use(sessions.Sessions("mysession", store))
	r.Use(cors.Default())

	gob.RegisterName(
		"github.com/aouyang1/go-matrixprofile/matrixprofile.MatrixProfile",
		matrixprofile.MatrixProfile{},
	)

	v1 := r.Group("/api/v1")
	{
		v1.GET("/data", getData)
		v1.POST("/calculate", calculateMP)
		v1.GET("/topkmotifs", topKMotifs)
		v1.GET("/topkdiscords", topKDiscords)
		v1.POST("/mp", getMP)
	}
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	r.Run(":" + port)
}

// initRedis initializes the connection to the redis store for caching session Matrix Profile data
func initRedis() (redis.Store, error) {
	if u := os.Getenv("REDIS_URL"); u != "" {
		// override global variable if environment variable present
		redisURL = u
	}

	store, err := redis.NewStore(10, "tcp", redisURL, "", []byte("secret"))
	if err != nil {
		return nil, err
	}

	err, rs := redis.GetRedisStore(store)
	if err != nil {
		return nil, err
	}
	rs.SetMaxLength(maxRedisBlobSize)
	rs.Options.MaxAge = retentionPeriod

	return store, nil
}

type Data struct {
	Data []float64 `json:"data"`
}

func fetchData() (Data, error) {
	jsonFile, err := os.Open("./penguin_data.json")
	if err != nil {
		return Data{}, err
	}

	byteValue, err := ioutil.ReadAll(jsonFile)
	if err != nil {
		return Data{}, err
	}

	var data Data
	if err := json.Unmarshal(byteValue, &data); err != nil {
		return Data{}, err
	}

	data.Data = smooth(data.Data, 21)[:24*60*7]

	return data, nil
}

// smooth performs a non causal averaging of neighboring data points
func smooth(data []float64, m int) []float64 {
	leftSpan := m / 2
	rightSpan := m / 2

	var sum float64
	var s, e int
	sdata := make([]float64, len(data))

	for i := range data {
		s = i - leftSpan
		if s < 0 {
			s = 0
		}

		e = i + rightSpan + 1
		if e > len(data) {
			e = len(data)
		}

		sum = 0
		for _, d := range data[s:e] {
			sum += d
		}

		sdata[i] = sum / float64(e-s)
	}
	return sdata
}

func getData(c *gin.Context) {
	endpoint := "/api/v1/data"
	method := "GET"
	data, err := fetchData()
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	c.Header("Content-Type", "application/json")
	buildCORSHeaders(c)

	requestTotal.WithLabelValues(method, endpoint, "200").Inc()
	c.JSON(200, data.Data)
}

type Segment struct {
	CAC []float64 `json:"cac"`
}

func calculateMP(c *gin.Context) {
	endpoint := "/api/v1/calculate"
	method := "POST"
	session := sessions.Default(c)
	buildCORSHeaders(c)

	params := struct {
		M int `json:"m"`
	}{}
	if err := c.BindJSON(&params); err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}
	m := params.M

	data, err := fetchData()
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	mp, err := matrixprofile.New(data.Data, nil, m)
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	if err = mp.Stomp(mpConcurrency); err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	// compute the corrected arc curve based on the current index matrix profile
	_, _, cac := mp.Segment()

	// cache matrix profile for current session
	session.Set("mp", &mp)
	session.Save()

	requestTotal.WithLabelValues(method, endpoint, "200").Inc()
	c.JSON(200, Segment{cac})
}

type Motif struct {
	Groups []matrixprofile.MotifGroup `json:"groups"`
	Series [][][]float64              `json:"series"`
}

func topKMotifs(c *gin.Context) {
	endpoint := "/api/v1/topkmotifs"
	method := "GET"
	session := sessions.Default(c)
	buildCORSHeaders(c)

	k, err := strconv.Atoi(c.Query("k"))
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	r, err := strconv.ParseFloat(c.Query("r"), 64)
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	v := session.Get("mp")

	var mp matrixprofile.MatrixProfile
	if v == nil {
		// either the cache expired or this was called directly
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{
			Error:        errors.New("matrix profile is not initialized to compute motifs"),
			CacheExpired: true,
		})
		return
	} else {
		mp = v.(matrixprofile.MatrixProfile)
	}
	motifGroups, err := mp.TopKMotifs(k, r)
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	var motif Motif
	motif.Groups = motifGroups
	motif.Series = make([][][]float64, len(motifGroups))
	for i, g := range motif.Groups {
		motif.Series[i] = make([][]float64, len(g.Idx))
		for j, midx := range g.Idx {
			motif.Series[i][j], err = matrixprofile.ZNormalize(mp.A[midx : midx+mp.M])
			if err != nil {
				requestTotal.WithLabelValues(method, endpoint, "500").Inc()
				c.JSON(500, RespError{Error: err})
				return
			}
		}
	}

	requestTotal.WithLabelValues(method, endpoint, "200").Inc()
	c.JSON(200, motif)
}

type Discord struct {
	Groups []int       `json:"groups"`
	Series [][]float64 `json:"series"`
}

func topKDiscords(c *gin.Context) {
	endpoint := "/api/v1/topkdiscords"
	method := "GET"
	session := sessions.Default(c)
	buildCORSHeaders(c)

	kstr := c.Query("k")

	k, err := strconv.Atoi(kstr)
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	v := session.Get("mp")
	var mp matrixprofile.MatrixProfile
	if v == nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{
			errors.New("matrix profile is not initialized to compute discords"),
			true,
		})
		return
	} else {
		mp = v.(matrixprofile.MatrixProfile)
	}
	discords, err := mp.TopKDiscords(k, mp.M/2)
	if err != nil {
		requestTotal.WithLabelValues(method, endpoint, "500").Inc()
		c.JSON(500, RespError{
			Error: errors.New("failed to compute discords"),
		})
		return
	}

	var discord Discord
	discord.Groups = discords
	discord.Series = make([][]float64, len(discords))
	for i, didx := range discord.Groups {
		discord.Series[i], err = matrixprofile.ZNormalize(mp.A[didx : didx+mp.M])
		if err != nil {
			requestTotal.WithLabelValues(method, endpoint, "500").Inc()
			c.JSON(500, RespError{Error: err})
			return
		}
	}

	requestTotal.WithLabelValues(method, endpoint, "200").Inc()
	c.JSON(200, discord)
}

type MP struct {
	AV         []float64 `json:"annotation_vector"`
	AdjustedMP []float64 `json:"adjusted_mp"`
}

func getMP(c *gin.Context) {
	endpoint := "/api/v1/mp"
	session := sessions.Default(c)
	buildCORSHeaders(c)

	params := struct {
		Name string `json:"name"`
	}{}
	if err := c.BindJSON(&params); err != nil {
		requestTotal.WithLabelValues("POST", endpoint, "500").Inc()
		c.JSON(500, RespError{
			Error: errors.New("failed to unmarshall POST parameters with field `name`"),
		})
		return
	}
	avname := params.Name

	v := session.Get("mp")
	var mp matrixprofile.MatrixProfile
	if v == nil {
		// matrix profile is not initialized so don't return any data back for the
		// annotation vector
		requestTotal.WithLabelValues("POST", endpoint, "500").Inc()
		c.JSON(500, RespError{
			Error:        errors.New("matrix profile is not initialized"),
			CacheExpired: true,
		})
		return
	} else {
		mp = v.(matrixprofile.MatrixProfile)
	}

	switch avname {
	case "default", "":
		mp.AV = matrixprofile.DefaultAV
	case "complexity":
		mp.AV = matrixprofile.ComplexityAV
	case "meanstd":
		mp.AV = matrixprofile.MeanStdAV
	case "clipping":
		mp.AV = matrixprofile.ClippingAV
	default:
		requestTotal.WithLabelValues("POST", endpoint, "500").Inc()
		c.JSON(500, RespError{
			Error: errors.New("invalid annotation vector name " + avname),
		})
		return
	}

	// cache matrix profile for current session
	session.Set("mp", &mp)
	session.Save()

	av, err := mp.GetAV()
	if err != nil {
		requestTotal.WithLabelValues("POST", endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	adjustedMP, err := mp.ApplyAV(av)
	if err != nil {
		requestTotal.WithLabelValues("POST", endpoint, "500").Inc()
		c.JSON(500, RespError{Error: err})
		return
	}

	requestTotal.WithLabelValues("POST", endpoint, "200").Inc()
	c.JSON(200, MP{AV: av, AdjustedMP: adjustedMP})
}

func buildCORSHeaders(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "http://localhost:8080")
	c.Header("Access-Control-Allow-Credentials", "true")
	c.Header("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	c.Header("Access-Control-Allow-Methods", "GET, POST")
}
