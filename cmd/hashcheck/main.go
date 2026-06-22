package main

import (
	"fmt"
	"os"
	"lukechampine.com/blake3"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil { panic(err) }
	h := blake3.Sum256(data)
	fmt.Printf("%x\n", h)
}
