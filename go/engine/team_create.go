// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package engine

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"

	"golang.org/x/crypto/nacl/box"
)

type TeamCreateEngine struct {
	libkb.Contextified
	name string
}

func NewTeamCreateEngine(g *libkb.GlobalContext, name string) *TeamCreateEngine {
	return &TeamCreateEngine{
		Contextified: libkb.NewContextified(g),
		name:         name,
	}
}

func (e *TeamCreateEngine) Name() string {
	return "NewTeam"
}

func (e *TeamCreateEngine) Prereqs() Prereqs {
	return Prereqs{
		Device: true,
	}
}

func (e *TeamCreateEngine) RequiredUIs() []libkb.UIKind {
	return []libkb.UIKind{
		libkb.LogUIKind,
		libkb.SecretUIKind,
	}
}

func (e *TeamCreateEngine) SubConsumers() []libkb.UIConsumer {
	return []libkb.UIConsumer{}
}

func (e *TeamCreateEngine) Run(ctx *Context) (err error) {
	defer e.G().CTrace(ctx.GetNetContext(), "TeamCreateEngine", func() error { return err })()

	me, err := libkb.LoadMe(libkb.NewLoadUserArg(e.G()))
	if err != nil {
		return err
	}

	deviceSigningKey, deviceEncryptionKey, err := e.getDeviceKeys(ctx, me)
	if err != nil {
		return err
	}

	perTeamSecret, perTeamSigningKey, perTeamEncryptionKey, err := generatePerTeamKeys()
	if err != nil {
		return err
	}

	ownerLatest := me.GetComputedKeyFamily().GetLatestPerUserKey()
	if ownerLatest == nil {
		return errors.New("can't create a new team without having provisioned a per-user key")
	}
	secretboxRecipients := map[string]keybase1.PerUserKey{
		me.GetName(): *ownerLatest,
	}
	// These boxes will get posted along with the sig below.
	secretboxes, err := boxTeamSharedSecret(perTeamSecret, deviceEncryptionKey, secretboxRecipients)
	if err != nil {
		return err
	}

	teamSection, err := makeRootTeamSectionNoReverse(e.name, me, perTeamSigningKey.GetKID(), perTeamEncryptionKey.GetKID())
	if err != nil {
		return err
	}

	// At this point the team section has every field filled out except the
	// reverse sig. Now we'll wrap it into a full sig, marshal it to JSON, and
	// sign it, *twice*. The first time with the per-team signing key, to
	// produce the reverse sig, and the second time with the device signing
	// key, after the reverse sig has been written in.

	sigBodyForReverse, err := me.TeamRootSig(deviceSigningKey, teamSection)
	if err != nil {
		return err
	}
	// Note that this (sigchain-v1-style) reverse sig is made with the derived *per-team* signing key.
	reverseSig, _, _, err := libkb.SignJSON(sigBodyForReverse, perTeamSigningKey)
	if err != nil {
		return err
	}
	teamSection.PerTeamKey.ReverseSig = &reverseSig

	// Update the team section to include the reverse sig, format it again, and make a sigchain-v2-style sig out of it.
	sigBodyAfterReverse, err := me.TeamRootSig(deviceSigningKey, teamSection)
	if err != nil {
		return err
	}
	sigJSONAfterReverse, err := sigBodyAfterReverse.Marshal()
	if err != nil {
		return err
	}
	v2Sig, err := makeSigchainV2OuterSig(
		deviceSigningKey,
		libkb.LinkTypeTeamRoot,
		1, /* seqno */
		sigJSONAfterReverse,
		nil,   /* prevLinkID */
		false, /* hasRevokes */
	)
	if err != nil {
		return err
	}

	sigMultiItem := libkb.SigMultiItem{
		Sig:        v2Sig,
		SigningKID: deviceSigningKey.GetKID().String(),
		Type:       string(libkb.LinkTypeTeamRoot),
		SigInner:   string(sigJSONAfterReverse),
		TeamID:     libkb.RootTeamIDFromName(e.name),
	}
	sigMultiItem.PublicKeys.Encryption = perTeamEncryptionKey.GetKID().String()
	sigMultiItem.PublicKeys.Signing = perTeamSigningKey.GetKID().String()

	payload := make(libkb.JSONPayload)
	payload["sigs"] = []interface{}{sigMultiItem}
	payload["per_team_key"] = secretboxes

	_, err = e.G().API.PostJSON(libkb.APIArg{
		Endpoint:    "sig/multi",
		SessionType: libkb.APISessionTypeREQUIRED,
		JSONPayload: payload,
	})
	if err != nil {
		return err
	}

	return nil
}

func (e *TeamCreateEngine) getDeviceKeys(ctx *Context, me *libkb.User) (sigKey libkb.NaclSigningKeyPair, encKey libkb.NaclDHKeyPair, err error) {
	sigSKA := libkb.SecretKeyArg{
		Me:      me,
		KeyType: libkb.DeviceSigningKeyType,
	}
	sigGenericKey, err := e.G().Keyrings.GetSecretKeyWithPrompt(ctx.SecretKeyPromptArg(sigSKA, "to create a new team"))
	if err != nil {
		return
	}
	if err = sigGenericKey.CheckSecretKey(); err != nil {
		return
	}
	sigKey, ok := sigGenericKey.(libkb.NaclSigningKeyPair)
	if !ok {
		err = fmt.Errorf("got an unexpected key type for device signing key: %T", sigGenericKey)
		return
	}

	encSKA := libkb.SecretKeyArg{
		Me:      me,
		KeyType: libkb.DeviceEncryptionKeyType,
	}
	encGenericKey, err := e.G().Keyrings.GetSecretKeyWithPrompt(ctx.SecretKeyPromptArg(encSKA, "to create a new team"))
	if err != nil {
		return
	}
	if err = encGenericKey.CheckSecretKey(); err != nil {
		return
	}
	encKey, ok = encGenericKey.(libkb.NaclDHKeyPair)
	if !ok {
		err = fmt.Errorf("got an unexpected key type for device encryption key: %T", encGenericKey)
		return
	}

	return
}

func generatePerTeamKeys() (sharedSecret []byte, signingKey libkb.NaclSigningKeyPair, encryptionKey libkb.NaclDHKeyPair, err error) {
	// This is the magical secret key, from which we derive a DH keypair and a
	// signing keypair.
	sharedSecret, err = libkb.RandBytes(32)
	if err != nil {
		return
	}
	encryptionKey, err = libkb.MakeNaclDHKeyPairFromSecretBytes(derivedSecret(sharedSecret, libkb.TeamDHDerivationString))
	if err != nil {
		return
	}
	signingKey, err = libkb.MakeNaclSigningKeyPairFromSecretBytes(derivedSecret(sharedSecret, libkb.TeamEdDSADerivationString))
	if err != nil {
		return
	}
	return
}

type PerTeamSharedSecretBoxes struct {
	Generation    int               `json:"generation"`
	EncryptingKid string            `json:"encrypting_kid"`
	Nonce         string            `json:"nonce"`
	Prev          string            `json:"prev"`
	Boxes         map[string]string `json:"boxes"`
}

func boxTeamSharedSecret(secret []byte, senderKey libkb.NaclDHKeyPair, recipients map[string]keybase1.PerUserKey) (*PerTeamSharedSecretBoxes, error) {
	noncePrefix, err := libkb.RandBytes(20)
	if err != nil {
		return nil, err
	}
	// The counter starts at 1, because 0 will be the prev secretbox, which is
	// omitted for the team root link, because this is the first shared key.
	var counter uint32 = 1
	boxes := make(map[string]string)
	for username, recipientPerUserKey := range recipients {
		recipientPerUserGenericKeypair, err := libkb.ImportKeypairFromKID(recipientPerUserKey.EncKID)
		if err != nil {
			return nil, err
		}
		recipientPerUserNaclKeypair, ok := recipientPerUserGenericKeypair.(libkb.NaclDHKeyPair)
		if !ok {
			return nil, fmt.Errorf("got an unexpected key type for recipient KID in sharedTeamKeyBox: %T", recipientPerUserGenericKeypair)
		}
		var nonce [24]byte
		counterBytes := [4]byte{}
		binary.BigEndian.PutUint32(counterBytes[:], counter)
		copy(nonce[:20], noncePrefix)
		copy(nonce[20:24], counterBytes[:])
		// TODO: pack [ 1, <per-user-seqno>, <nonce-bottom-4-bytes>, <box of current shared team secret> ]
		ctext := box.Seal(nil, secret, &nonce, ((*[32]byte)(&recipientPerUserNaclKeypair.Public)), ((*[32]byte)(senderKey.Private)))
		boxArray := []interface{}{libkb.SharedTeamKeyBoxVersion1, recipientPerUserKey.Seqno, counterBytes[:], ctext}
		encodedArray, err := libkb.MsgpackEncode(boxArray)
		if err != nil {
			return nil, err
		}
		base64Array := base64.StdEncoding.EncodeToString(encodedArray)
		boxes[username] = base64Array
	}

	return &PerTeamSharedSecretBoxes{
		Generation:    1,
		EncryptingKid: string(senderKey.GetKID()),
		Nonce:         base64.StdEncoding.EncodeToString(noncePrefix),
		Prev:          "", // no prev for the first team link
		Boxes:         boxes,
	}, nil
}

func makeRootTeamSectionNoReverse(teamName string, owner *libkb.User, perTeamSigningKID keybase1.KID, perTeamEncryptionKID keybase1.KID) (libkb.TeamSection, error) {
	teamID := libkb.RootTeamIDFromName(teamName)
	teamSection := libkb.TeamSection{
		Name: teamName,
		ID:   teamID,
	}

	ownerName, err := libkb.NameWithEldestSeqno(owner.GetName(), owner.GetCurrentEldestSeqno())
	// An error happens here if the seqno isn't loaded for some reason.
	if err != nil {
		return teamSection, err
	}

	teamSection.Members.Owner = []string{ownerName}
	teamSection.Members.Admin = []string{}
	teamSection.Members.Writer = []string{}
	teamSection.Members.Reader = []string{}
	teamSection.PerTeamKey.Generation = 1
	teamSection.PerTeamKey.SigningKID = string(perTeamSigningKID)
	teamSection.PerTeamKey.EncryptionKID = string(perTeamEncryptionKID)

	// At this point the team section has every field filled out except the
	// reverse sig. Now we'll wrap it into a full sig, marshal it to JSON, and
	// sign it, *twice*. The first time with the per-team signing key, to
	// produce the reverse sig, and the second time with the device signing
	// key, after the reverse sig has been written in.

	return teamSection, nil
}

func derivedSecret(secret []byte, context string) []byte {
	digest := hmac.New(sha512.New, secret)
	digest.Write([]byte(context))
	return digest.Sum(nil)[:32]
}

func makeSigchainV2OuterSig(
	signingKey libkb.GenericKey,
	v1LinkType libkb.LinkType,
	seqno libkb.Seqno,
	innerLinkJSON []byte,
	prevLinkID libkb.LinkID,
	hasRevokes bool,
) (
	string,
	error,
) {
	linkID := libkb.ComputeLinkID(innerLinkJSON)

	v2LinkType, err := libkb.SigchainV2TypeFromV1TypeAndRevocations(string(v1LinkType), hasRevokes)
	if err != nil {
		return "", err
	}

	outerLink := libkb.OuterLinkV2{
		Version:  2,
		Seqno:    seqno,
		Prev:     nil,
		Curr:     linkID,
		LinkType: v2LinkType,
	}
	encodedOuterLink, err := outerLink.Encode()
	if err != nil {
		return "", err
	}

	sig, _, err := signingKey.SignToString(encodedOuterLink)
	if err != nil {
		return "", err
	}

	return sig, nil
}
