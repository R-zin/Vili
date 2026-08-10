package auth

import (
	"log"

	"golang.org/x/crypto/bcrypt"
)

func hash(password string) string {
	c := bcrypt.DefaultCost
	ps := []byte(password)
	gen_hash, err := bcrypt.GenerateFromPassword(ps, c)
	if err != nil {
		log.Fatal(err)
	}
	return string(gen_hash)

}

func comp_hash(password string, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(password), []byte(hash))
	return err == nil
}
