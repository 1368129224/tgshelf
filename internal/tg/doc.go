// Package tg wraps low-level communication with the Telegram MTProto API and provides session
// management and authentication support.
//
// For the login flow see login.go: SendCode (send the code) -> SignIn (verify the code, which may
// require 2FA) -> CheckPassword (2FA password); Self queries the current account.
package tg
