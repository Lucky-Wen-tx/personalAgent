package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClient 所有天气请求共用，带 15 秒超时
var httpClient = &http.Client{Timeout: 15 * time.Second}

// NewGetWeather 真实天气查询工具（Open-Meteo，免费无需 key）
func NewGetWeather() *ToolDef {
	return &ToolDef{
		Name:        "get_weather",
		Description: "查询指定城市某天的天气情况（支持今天起未来16天），返回温度区间和天气描述",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{
					"type":        "string",
					"description": "城市名称，例如：深圳、北京、上海",
				},
				"date": map[string]any{
					"type":        "string",
					"description": "日期，格式 YYYY-MM-DD，缺省为明天，只支持今天起未来16天内",
				},
			},
			"required": []string{"city"},
		},
		Handler: func(ctx context.Context, args string) (string, error) {
			var p struct {
				City string `json:"city"`
				Date string `json:"date"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", fmt.Errorf("参数解析失败: %w", err)
			}
			if p.City == "" {
				return "", fmt.Errorf("缺少必填参数 city")
			}

			loc, err := geocode(ctx, p.City)
			if err != nil {
				return "", err
			}

			fc, err := fetchForecast(ctx, loc)
			if err != nil {
				return "", err
			}

			// 缺省查明天：daily.time[1] 是当地时区的明天
			date := p.Date
			if date == "" {
				if len(fc.Daily.Time) > 1 {
					date = fc.Daily.Time[1]
				} else {
					date = fc.Daily.Time[0]
				}
			}

			idx := -1
			for i, d := range fc.Daily.Time {
				if d == date {
					idx = i
					break
				}
			}
			if idx < 0 {
				return "", fmt.Errorf("只能查询今天起未来16天内的天气（%s 不在预报范围内）", date)
			}

			cond := weatherDesc(fc.Daily.WeatherCode[idx])
			res := weatherResult{
				City:      loc.Name,
				Date:      date,
				Condition: cond,
				TempLow:   fc.Daily.TempMin[idx],
				TempHigh:  fc.Daily.TempMax[idx],
				Wind:      fmt.Sprintf("%s风 %.0f km/h", windDir(fc.Daily.WindDir[idx]), fc.Daily.WindMax[idx]),
				Tip:       weatherTip(cond, fc.Daily.TempMin[idx], fc.Daily.TempMax[idx], fc.Daily.PrecipProb[idx]),
			}
			out, err := json.Marshal(res)
			if err != nil {
				return "", fmt.Errorf("结果序列化失败: %w", err)
			}
			return string(out), nil
		},
	}
}

// ---------- 内部实现 ----------

type location struct {
	Name string
	Lat  float64
	Lon  float64
}

// geocode 把城市名解析成经纬度，优先匹配中国大陆
func geocode(ctx context.Context, city string) (location, error) {
	u := "https://geocoding-api.open-meteo.com/v1/search?name=" +
		url.QueryEscape(city) + "&count=5&language=zh&format=json"

	var gr struct {
		Results []struct {
			Name        string  `json:"name"`
			Latitude    float64 `json:"latitude"`
			Longitude   float64 `json:"longitude"`
			CountryCode string  `json:"country_code"`
		} `json:"results"`
	}
	if err := getJSON(ctx, u, &gr); err != nil {
		return location{}, err
	}
	if len(gr.Results) == 0 {
		return location{}, fmt.Errorf("未找到城市「%s」，请检查城市名称", city)
	}
	// 同名城市（如深圳/新竹）优先选中国大陆的
	for _, r := range gr.Results {
		if r.CountryCode == "CN" {
			return location{Name: r.Name, Lat: r.Latitude, Lon: r.Longitude}, nil
		}
	}
	return location{Name: gr.Results[0].Name, Lat: gr.Results[0].Latitude, Lon: gr.Results[0].Longitude}, nil
}

type forecast struct {
	Daily struct {
		Time        []string  `json:"time"`
		TempMax     []float64 `json:"temperature_2m_max"`
		TempMin     []float64 `json:"temperature_2m_min"`
		WeatherCode []int     `json:"weather_code"`
		WindMax     []float64 `json:"wind_speed_10m_max"`
		WindDir     []float64 `json:"wind_direction_10m_dominant"`
		PrecipProb  []float64 `json:"precipitation_probability_max"`
	} `json:"daily"`
}

func fetchForecast(ctx context.Context, loc location) (forecast, error) {
	u := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&timezone=auto&forecast_days=16&daily=temperature_2m_max,temperature_2m_min,weather_code,wind_speed_10m_max,wind_direction_10m_dominant,precipitation_probability_max",
		loc.Lat, loc.Lon,
	)
	var fc forecast
	if err := getJSON(ctx, u, &fc); err != nil {
		return forecast{}, err
	}
	return fc, nil
}

// getJSON 发 GET 请求并把 JSON 响应解码到 v
func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("天气服务请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("天气服务返回 HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("解析天气服务响应失败: %w", err)
	}
	return nil
}

// weatherResult 是给模型看的工具结果，字段与之前模拟版保持一致
type weatherResult struct {
	City      string  `json:"city"`
	Date      string  `json:"date"`
	Condition string  `json:"condition"`
	TempLow   float64 `json:"temp_low"`
	TempHigh  float64 `json:"temp_high"`
	Wind      string  `json:"wind"`
	Tip       string  `json:"tip"`
}

// weatherDescMap 把 WMO 天气代码翻译成中文描述
var weatherDescMap = map[int]string{
	0: "晴", 1: "大致晴", 2: "局部多云", 3: "阴",
	45: "雾", 48: "雾凇",
	51: "毛毛雨", 53: "毛毛雨", 55: "浓密毛毛雨",
	56: "冻毛毛雨", 57: "浓密冻毛毛雨",
	61: "小雨", 63: "中雨", 65: "大雨",
	66: "冻雨", 67: "强冻雨",
	71: "小雪", 73: "中雪", 75: "大雪", 77: "雪粒",
	80: "阵雨", 81: "中阵雨", 82: "强阵雨",
	85: "阵雪", 86: "强阵雪",
	95: "雷阵雨", 96: "雷阵雨伴冰雹", 99: "强雷阵雨伴冰雹",
}

func weatherDesc(code int) string {
	if d, ok := weatherDescMap[code]; ok {
		return d
	}
	return "未知"
}

// windDir 把风向角度（0=北，90=东）转成中文方位
func windDir(deg float64) string {
	dirs := []string{"北", "东北", "东", "东南", "南", "西南", "西", "西北"}
	return dirs[int(deg/45+0.5)%8]
}

// weatherTip 根据天气和温度生成出行提示
func weatherTip(cond string, low, high, precip float64) string {
	var tips []string
	if precip >= 50 || strings.Contains(cond, "雨") || strings.Contains(cond, "雪") {
		tips = append(tips, "出门记得带伞")
	}
	switch {
	case high >= 30:
		tips = append(tips, "白天炎热，注意防晒补水")
	case high >= 25:
		tips = append(tips, "白天温暖，短袖薄衫即可")
	case high >= 18:
		tips = append(tips, "白天舒适，长袖或薄外套")
	default:
		tips = append(tips, "天气偏冷，注意保暖")
	}
	if low <= 10 {
		tips = append(tips, "早晚温差大，建议加外套")
	}
	if len(tips) == 0 {
		tips = append(tips, "天气宜人，适合出行")
	}
	return strings.Join(tips, "；")
}
