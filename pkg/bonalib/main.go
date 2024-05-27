package bonalib

import (
	"math/rand"
	"strconv"

	// "log"
	"github.com/davecgh/go-spew/spew"
	// "log"
	"fmt"
)

func Baka() string {
	return "Baka"
}

func RandNumber() string {
	return strconv.Itoa(rand.Intn(1000))
}

func Log(msg string, obj interface{}) {
	// 32: green 33: yellow
	color := "\033[1;33m%v\033[0m" // yellow
	str := spew.Sprintln("0---bonaLog", msg, obj)
	fmt.Printf(color, str)
	color = "\033[0m%v\033[0m" // reset
	fmt.Printf(color, "\n")
}
