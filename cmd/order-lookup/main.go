// Command order-lookup prints a few fields of named orders, read-only. Used
// to reconcile Shiprocket's ledger against what we actually charged the
// customer.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
)

func main() {
	numbers := flag.String("orders", "", "comma-separated order numbers")
	flag.Parse()

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	_, db, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for _, num := range strings.Split(*numbers, ",") {
		num = strings.TrimSpace(num)
		if num == "" {
			continue
		}
		var doc map[string]interface{}
		err := db.Collection("orders").FindOne(ctx, map[string]interface{}{"order_number": num}).Decode(&doc)
		if err != nil {
			fmt.Printf("%s: not found (%v)\n", num, err)
			continue
		}
		pi, _ := doc["payment_info"].(map[string]interface{})
		method := ""
		if pi != nil {
			method, _ = pi["method"].(string)
		}
		fmt.Printf("%s: total=%v method=%v status=%v shipping=%v weight=%v\n",
			num, doc["total"], method, doc["status"], doc["shipping_charge"], doc["weight"])
	}
}

func countItems(v interface{}) int {
	if arr, ok := v.([]interface{}); ok {
		return len(arr)
	}
	return 0
}
