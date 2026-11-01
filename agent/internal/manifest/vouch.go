package manifest

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Signing domains of the join flow.
const (
	DomainInvite       = "invite"
	DomainConfirmation = "invite-confirmation"
)

// Invite is what an inviter's device signs and wraps in an invite link. The
// manifest keeps nonce and expiry so the signature can be checked again by
// anyone; the applicant's name and allocation are the member entry's.
type Invite struct {
	Network      string       `cbor:"network"`
	Inviter      identity.Key `cbor:"inviter"` // the inviter's member key
	Nonce        []byte       `cbor:"nonce"`
	Expiry       time.Time    `cbor:"expiry"`
	Applicant    string       `cbor:"applicant,omitempty"`
	AllocationGB int          `cbor:"allocation_gb,omitempty"`
}

// Confirmation is what the inviter's device signs after comparing the
// applicant's display code over the phone.
type Confirmation struct {
	Network   string       `cbor:"network"`
	Inviter   identity.Key `cbor:"inviter"`
	Applicant identity.Key `cbor:"applicant"` // the new member's key
	Nonce     []byte       `cbor:"nonce"`
}

// SignInvite and SignConfirmation are the inviter's device's two acts.
func SignInvite(dev *identity.DeviceKey, inv Invite) ([]byte, error) {
	inv.Expiry = inv.Expiry.UTC().Truncate(time.Second)
	body, err := identity.Marshal(inv)
	if err != nil {
		return nil, err
	}
	return dev.Sign(DomainInvite, body), nil
}

// SignConfirmation signs the confirmation.
func SignConfirmation(dev *identity.DeviceKey, c Confirmation) ([]byte, error) {
	body, err := identity.Marshal(c)
	if err != nil {
		return nil, err
	}
	return dev.Sign(DomainConfirmation, body), nil
}

// NewVouch builds the manifest entry from the inviter's certificate and the
// two signatures.
func NewVouch(inviterName string, cert *identity.SignedCertificate, inv Invite, inviteSig, confirmSig []byte) (Vouch, error) {
	c, err := cert.Encode()
	if err != nil {
		return Vouch{}, err
	}
	e := base64.StdEncoding.EncodeToString
	return Vouch{Member: inviterName, DeviceCert: e(c), Nonce: e(inv.Nonce), Expiry: inv.Expiry.UTC().Truncate(time.Second).Format(time.RFC3339),
		InviteSig: e(inviteSig), ConfirmSig: e(confirmSig)}, nil
}

// Verify checks a vouch against the inviter's and the applicant's entries:
// the device certificate is the inviter's and verifies, and both signatures
// were made by that device over the invite and the confirmation.
func (v Vouch) Verify(network string, inviter, applicant *Member) error {
	d := base64.StdEncoding.DecodeString
	certBytes, err := d(v.DeviceCert)
	if err != nil {
		return fmt.Errorf("device_cert: %w", err)
	}
	cert, err := identity.DecodeCertificate(certBytes)
	if err != nil {
		return err
	}
	if cert.Certificate.Member != inviter.Key {
		return errors.New("the device certificate is not the inviter's")
	}
	if cert.Certificate.Network != network {
		return errors.New("the device certificate is for another network")
	}
	nonce, err := d(v.Nonce)
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	expiry, err := time.Parse(time.RFC3339, v.Expiry)
	if err != nil {
		return fmt.Errorf("expiry: %w", err)
	}
	inviteSig, err := d(v.InviteSig)
	if err != nil {
		return fmt.Errorf("invite_sig: %w", err)
	}
	confirmSig, err := d(v.ConfirmSig)
	if err != nil {
		return fmt.Errorf("confirm_sig: %w", err)
	}
	inv := Invite{Network: network, Inviter: inviter.Key, Nonce: nonce, Expiry: expiry.UTC().Truncate(time.Second), Applicant: applicant.Name}
	if applicant.AllocationGB != nil {
		inv.AllocationGB = *applicant.AllocationGB
	}
	body, err := identity.Marshal(inv)
	if err != nil {
		return err
	}
	if err := identity.VerifyDevice(cert.Certificate.Device, DomainInvite, body, inviteSig); err != nil {
		return fmt.Errorf("invite: %w", err)
	}
	body, err = identity.Marshal(Confirmation{Network: network, Inviter: inviter.Key, Applicant: applicant.Key, Nonce: nonce})
	if err != nil {
		return err
	}
	if err := identity.VerifyDevice(cert.Certificate.Device, DomainConfirmation, body, confirmSig); err != nil {
		return fmt.Errorf("confirmation: %w", err)
	}
	return nil
}
