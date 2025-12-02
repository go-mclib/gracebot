package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	openrouter "github.com/revrost/go-openrouter"
)

const weatherAPIURL = "https://wttr.in/%s?format=j1"

func getWeatherTool() openrouter.Tool {
	return openrouter.Tool{
		Type: openrouter.ToolTypeFunction,
		Function: &openrouter.FunctionDefinition{
			Name:        "get_weather",
			Description: "Get current weather conditions for a specified location. Use this when users ask about weather, temperature, or conditions in a city.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"location": map[string]interface{}{
						"type":        "string",
						"description": "The city or location name (e.g., 'London', 'New York', 'Tokyo')",
					},
				},
				"required": []string{"location"},
			},
		},
	}
}

type weatherData struct {
	CurrentCondition []struct {
		TempC       string `json:"temp_C"`
		TempF       string `json:"temp_F"`
		WeatherDesc []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
		Humidity       string `json:"humidity"`
		FeelsLikeC     string `json:"FeelsLikeC"`
		FeelsLikeF     string `json:"FeelsLikeF"`
		WindspeedKmph  string `json:"windspeedKmph"`
		WindspeedMiles string `json:"windspeedMiles"`
	} `json:"current_condition"`
	NearestArea []struct {
		AreaName []struct {
			Value string `json:"value"`
		} `json:"areaName"`
		Country []struct {
			Value string `json:"value"`
		} `json:"country"`
	} `json:"nearest_area"`
}

func executeGetWeather(arguments string) (string, error) {
	// parse arguments
	var args struct {
		Location string `json:"location"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.Location == "" {
		return "", fmt.Errorf("location is required")
	}

	// call API
	url := fmt.Sprintf(weatherAPIURL, args.Location)
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch weather: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("weather API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// parse data
	var weather weatherData
	if err := json.Unmarshal(body, &weather); err != nil {
		return "", fmt.Errorf("failed to parse weather data: %w", err)
	}

	// format response
	if len(weather.CurrentCondition) == 0 {
		return "", fmt.Errorf("no weather data available for %s", args.Location)
	}

	current := weather.CurrentCondition[0]
	location := args.Location
	if len(weather.NearestArea) > 0 && len(weather.NearestArea[0].AreaName) > 0 {
		location = weather.NearestArea[0].AreaName[0].Value
		if len(weather.NearestArea[0].Country) > 0 {
			location += ", " + weather.NearestArea[0].Country[0].Value
		}
	}

	description := "Clear"
	if len(current.WeatherDesc) > 0 {
		description = current.WeatherDesc[0].Value
	}

	result := fmt.Sprintf(
		"Weather in %s: %s, %s°C (%s°F), feels like %s°C (%s°F), humidity %s%%, wind %s km/h",
		location,
		description,
		current.TempC,
		current.TempF,
		current.FeelsLikeC,
		current.FeelsLikeF,
		current.Humidity,
		current.WindspeedKmph,
	)

	return result, nil
}
