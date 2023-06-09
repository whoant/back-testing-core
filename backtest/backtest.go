package backtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"new-back-testing/internal/redis_wrapper"
)

type BackTest struct {
	myStrategy      Strategy
	DataHandler     *DataHandler
	BackTestOptions *BacktestOptions
	order           []*Order
}

func NewBackTest(myStrategy Strategy, handler *DataHandler, options *BacktestOptions) *BackTest {
	return &BackTest{
		myStrategy:      myStrategy,
		DataHandler:     handler,
		BackTestOptions: options,
		order:           make([]*Order, 0),
	}
}

func (bt *BackTest) Run() {
	exchangeHandler := NewExchangeHandler(0)
	for _, price := range bt.DataHandler.Prices {
		if exchangeHandler.CountEnabledOrder() != 0 {
			for _, currentOrder := range exchangeHandler.HistoryOrders {
				if currentOrder.IsEnable() && currentOrder.CancelOrderPrice >= price.LowPrice() && currentOrder.CancelOrderPrice <= price.HighPrice() {
					currentOrder.MarkCancel(price.Timestamp())
					if currentOrder.OrderType == ASK {
						bt.BackTestOptions.CurrentBaseAmount += currentOrder.Amount
					} else {
						bt.BackTestOptions.CurrentQuoteAmount += currentOrder.Amount * currentOrder.OpenPrice
					}
				}

				if currentOrder.IsEnable() && currentOrder.TakeProfitPrice >= price.LowPrice() && currentOrder.TakeProfitPrice <= price.HighPrice() {
					currentOrder.MarkMatched(price.Timestamp())
					if currentOrder.OrderType == ASK {
						bt.BackTestOptions.CurrentQuoteAmount += currentOrder.Amount * currentOrder.OpenPrice
					} else {
						bt.BackTestOptions.CurrentBaseAmount += currentOrder.Amount / currentOrder.OpenPrice
					}
				}
			}
		}

		openingOrder := bt.myStrategy.OpenNewOrder(price)
		if openingOrder == nil {
			continue
		}

		amountPerBaseOrder := bt.BackTestOptions.AmountPerOrder
		amountCanSell := amountPerBaseOrder / price.OpenPrice()
		if openingOrder.OrderType == ASK && amountCanSell <= bt.BackTestOptions.CurrentBaseAmount {
			bt.BackTestOptions.CurrentBaseAmount -= amountCanSell
			newOrder := NewOrder(openingOrder, amountCanSell, price.OpenPrice(), price.Timestamp())

			exchangeHandler.HistoryOrders = append(exchangeHandler.HistoryOrders, newOrder)
		}
		if openingOrder.OrderType == BID && amountPerBaseOrder <= bt.BackTestOptions.CurrentQuoteAmount {
			bt.BackTestOptions.CurrentQuoteAmount -= amountPerBaseOrder
			newOrder := NewOrder(openingOrder, amountPerBaseOrder, price.OpenPrice(), price.Timestamp())

			exchangeHandler.HistoryOrders = append(exchangeHandler.HistoryOrders, newOrder)
		}
	}
	bt.order = exchangeHandler.HistoryOrders
}

func (bt *BackTest) Portfolio() {
	options := bt.BackTestOptions
	coins := strings.Split(options.Pair, "/")
	baseCoin, quoteCoin := coins[0], coins[1]

	initialSumAmount := options.InitialBaseAmount*bt.DataHandler.Prices[0].OpenPrice() + options.InitialQuoteAmount
	currentSumAmount := options.CurrentBaseAmount*bt.DataHandler.Prices[0].OpenPrice() + options.CurrentQuoteAmount

	profit := currentSumAmount - initialSumAmount
	profitMargin := profit / initialSumAmount * 100

	l := len(bt.DataHandler.Prices)
	startDate := bt.DataHandler.Prices[0].Timestamp()
	endDate := bt.DataHandler.Prices[l-1].Timestamp()

	cagr := calculateCAGR(startDate, endDate, initialSumAmount, currentSumAmount)

	duration := bt.DataHandler.Prices[l-1].Timestamp().Sub(bt.DataHandler.Prices[l-2].Timestamp())
	minutes := duration.Minutes()

	portfolio := &Portfolio{
		Pair:               options.Pair,
		BaseCoin:           baseCoin,
		QuoteCoin:          quoteCoin,
		AmountPerOrder:     options.AmountPerOrder,
		InitialBaseAmount:  options.InitialBaseAmount,
		CurrentBaseAmount:  options.CurrentBaseAmount,
		InitialQuoteAmount: options.InitialQuoteAmount,
		CurrentQuoteAmount: options.CurrentQuoteAmount,
		InitialSumAmount:   initialSumAmount,
		CurrentSumAmount:   currentSumAmount,
		Profit:             profit,
		ProfitMargin:       profitMargin,
		Cagr:               cagr,
		Orders:             bt.order,
		CreatedAt:          time.Now(),
		Strategy:           bt.myStrategy.Naming(),
		Prices:             bt.DataHandler.Prices,
		Interval:           minute2Interval(int(minutes)),
		From:               startDate,
		To:                 endDate,
	}

	res, err := json.Marshal(portfolio)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot marshal portfolio")
	}

	log.Info().Str("type", "portfolio").Str("pair", options.Pair).
		Str("base_coin", baseCoin).
		Str("quote_coin", quoteCoin).
		Float64("amount_per_order", options.AmountPerOrder).
		Float64("initial_base_amount", options.InitialBaseAmount).
		Float64("current_base_amount", options.CurrentBaseAmount).
		Float64("initial_quote_amount", options.InitialQuoteAmount).
		Float64("current_quote_amount", options.CurrentQuoteAmount).
		Float64("initial_sum_amount", initialSumAmount).
		Float64("current_sum_amount", currentSumAmount).
		Float64("profit", profit).
		Float64("profit_margin", profitMargin).
		Float64("cagr", cagr).
		Msg("calculate portfolio")

	key := fmt.Sprintf("%v_%v_%v_%v_%v", baseCoin, quoteCoin, replaceNonAlphabeticCharacterToDash(bt.myStrategy.Naming()), startDate.Unix(), endDate.Unix())
	storeData(key, string(res))
}

func calculateCAGR(startDate time.Time, endDate time.Time, initialAmount float64, currentAmount float64) float64 {
	years := endDate.Sub(startDate).Hours() / 24 / 365.25
	cagr := math.Pow(currentAmount/initialAmount, 1/years) - 1

	return cagr * 100
}

func storeData(key string, data string) {
	dataKey := fmt.Sprintf("data:%v", key)

	ctx := context.Background()
	redisClient := setupRedis()
	redisWrapper := redis_wrapper.NewRedisWrapper(redisClient, ctx)
	_, err := redisWrapper.IsExist(dataKey)
	if err != nil {
		log.Fatal().Err(err).Msg("failed check data")
	}

	versionKey := fmt.Sprintf("data:version:%v", key)
	currentVersion, err := redisWrapper.Client.Incr(ctx, versionKey).Result()
	if err != nil {
		log.Fatal().Err(err).Msg("failed get current version")
	}

	key = fmt.Sprintf("%v_%v", dataKey, currentVersion)
	err = redisWrapper.SetWithoutDuration(key, data)
	if err != nil {
		log.Fatal().Err(err).Msg("failed set data")
	}
	log.Info().Msg("set data successfully")
}

func setupRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "0.0.0.0:6380",
		DB:   3,
	})
}

func replaceNonAlphabeticCharacterToDash(str string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return r
		} else if unicode.IsDigit(r) {
			return r
		} else {
			return '-'
		}
	}, strings.ToLower(str))
}

func minute2Interval(m int) string {
	interval := map[int]string{
		1:   "1m",
		2:   "2m",
		3:   "3m",
		5:   "5m",
		60:  "1h",
		120: "2h",
	}

	if val, ok := interval[m]; ok {
		return val
	}

	return fmt.Sprintf("%vm", m)
}
