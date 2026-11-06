package main

import (
	"errors"
	"os"
	"strings"

	"github.com/crizzy9/plano/internal/push"
)

func pushConfig() (push.Sender, error) {
	var sender push.Sender
	sandbox, err := flag("PLANO_APNS_SANDBOX", false)
	if err != nil {
		return sender, err
	}
	apns := push.APNsConfig{
		TeamID:   os.Getenv("PLANO_APNS_TEAM_ID"),
		KeyID:    os.Getenv("PLANO_APNS_KEY_ID"),
		BundleID: os.Getenv("PLANO_APNS_BUNDLE_ID"),
		Sandbox:  sandbox,
	}
	const apnsKeyFile = "PLANO_APNS_PRIVATE_KEY_FILE"
	apnsHasKeyFile := os.Getenv(apnsKeyFile) != ""
	if apns.TeamID != "" || apns.KeyID != "" || apns.BundleID != "" || apnsHasKeyFile || sandbox {
		if strings.TrimSpace(apns.TeamID) == "" || strings.TrimSpace(apns.KeyID) == "" ||
			strings.TrimSpace(apns.BundleID) == "" || !apnsHasKeyFile {
			return sender, errors.New("APNs requires PLANO_APNS_TEAM_ID, PLANO_APNS_KEY_ID, PLANO_APNS_BUNDLE_ID and PLANO_APNS_PRIVATE_KEY_FILE")
		}
		apns.PrivateKeyPEM, err = secret("", apnsKeyFile)
		if err != nil {
			return sender, err
		}
		sender.APNs, err = push.NewAPNs(apns)
		if err != nil {
			return sender, err
		}
	}

	constrained, err := flag("PLANO_FCM_BANDWIDTH_CONSTRAINED", false)
	if err != nil {
		return sender, err
	}
	fcm := push.FCMConfig{ProjectID: os.Getenv("PLANO_FCM_PROJECT_ID"), BandwidthConstrained: constrained}
	const fcmAccountFile = "PLANO_FCM_SERVICE_ACCOUNT_FILE"
	fcmHasAccountFile := os.Getenv(fcmAccountFile) != ""
	if fcm.ProjectID != "" || fcmHasAccountFile || constrained {
		if !fcmHasAccountFile {
			return sender, errors.New("FCM requires PLANO_FCM_SERVICE_ACCOUNT_FILE")
		}
		fcm.ServiceAccountJSON, err = secret("", fcmAccountFile)
		if err != nil {
			return sender, err
		}
		sender.FCM, err = push.NewFCM(fcm)
		if err != nil {
			return sender, err
		}
	}
	return sender, nil
}
