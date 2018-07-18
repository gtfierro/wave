package iapi

import (
	"context"
	"crypto/rand"

	"github.com/immesys/asn1"
	"github.com/immesys/wave/serdes"
	"github.com/immesys/wave/wve"
)

type PEncryptMessage struct {
	Encryptor *EntitySecrets
	//Direct encryption key
	Subject *Entity
	//OAQUE encryption
	Namespace         *Entity
	NamespaceLocation LocationSchemeInstance
	Partition         [][]byte

	Content []byte
}
type REncryptMessage struct {
	Ciphertext []byte
}

func EncryptMessage(ctx context.Context, p *PEncryptMessage) (*REncryptMessage, wve.WVE) {
	if len(p.Content) == 0 {
		return nil, wve.Err(wve.InvalidParameter, "message to be encrypted is empty")
	}
	contentKey := make([]byte, 16+12)
	rand.Read(contentKey)
	contentCiphertext := aesGCMEncrypt(contentKey[:16], p.Content, contentKey[16:])

	canonicalForm := serdes.WaveEncryptedMessage{}
	canonicalForm.Contents = contentCiphertext

	if p.Subject != nil {
		key, err := p.Subject.WR1_DirectEncryptionKey()
		if err != nil {
			return nil, wve.Err(wve.InvalidParameter, "subject has no direct encryption key")
		}
		contentKeyCiphertext, err := key.EncryptMessage(ctx, contentKey)
		directKey := serdes.MessageKeyCurve25519ECDH{
			Ciphertext: contentKeyCiphertext,
		}
		canonicalForm.Keys = append(canonicalForm.Keys, asn1.NewExternal(directKey))
	}
	if p.Namespace != nil {
		if len(p.Partition) > 20 {
			return nil, wve.Err(wve.InvalidParameter, "partition longer than 20 elements")
		}
		partition := make([][]byte, 20)
		for i, e := range p.Partition {
			partition[i] = e
		}
		outerkey, err := p.Namespace.WR1_DomainVisiblityParams()
		if err != nil {
			return nil, wve.Err(wve.InvalidParameter, "namespace missing WR1 parameters")
		}
		innerkey, err := p.Namespace.WR1_BodyParams()
		if err != nil {
			return nil, wve.Err(wve.InvalidParameter, "namespace missing WR1 parameters")
		}
		wr1Key := serdes.MessageKeyWR1{}
		ns := p.Namespace.Keccak256HI().CanonicalForm()
		wr1Key.Namespace = *ns
		nsloc := p.NamespaceLocation.CanonicalForm()
		wr1Key.NamespaceLocation = *nsloc
		wr1Envelope := serdes.MessageKeyWR1Envelope{
			Partition: partition,
		}
		oaqueKey, err := innerkey.GenerateChildKey(ctx, partition)
		if err != nil {
			panic(err)
		}
		oaqueCiphertext, err := oaqueKey.EncryptMessage(ctx, contentKey)
		if err != nil {
			panic(err)
		}
		wr1Envelope.ContentsKey = oaqueCiphertext
		der, err := asn1.Marshal(wr1Envelope)
		if err != nil {
			panic(err)
		}
		envelopeKey := make([]byte, 16+12)
		rand.Read(envelopeKey)
		encryptedEnvelope := aesGCMEncrypt(envelopeKey[:16], der, envelopeKey[16:])
		wr1Key.Envelope = encryptedEnvelope
		envelopeKeyCiphertextKey, err := outerkey.GenerateChildKey(ctx, []byte(p.Namespace.Keccak256HI().MultihashString()))
		if err != nil {
			panic(err)
		}
		envelopeKeyCiphertext, err := envelopeKeyCiphertextKey.EncryptMessage(ctx, envelopeKey)
		if err != nil {
			panic(err)
		}
		wr1Key.EnvelopeKeyIBEBN256 = envelopeKeyCiphertext
		canonicalForm.Keys = append(canonicalForm.Keys, asn1.NewExternal(wr1Key))
	}

	wireObject := serdes.WaveWireObject{
		Content: asn1.NewExternal(canonicalForm),
	}
	der, err := asn1.Marshal(wireObject.Content)
	if err != nil {
		panic(err)
	}
	return &REncryptMessage{
		Ciphertext: der,
	}, nil
}

type WR1MessageDecryptionContext interface {
	WR1OAQUEKeysForContent(ctx context.Context, dst HashSchemeInstance, slots [][]byte, onResult func(k SlottedSecretKey) bool) error
	WR1IBEKeysForPartitionLabel(ctx context.Context, dst HashSchemeInstance, onResult func(k EntitySecretKeySchemeInstance) bool) error
	WR1DirectDecryptionKey(ctx context.Context, dst HashSchemeInstance, onResult func(k EntitySecretKeySchemeInstance) bool) error
}

type PDecryptMessage struct {
	Decryptor  *EntitySecrets
	Ciphertext []byte
	Dctx       WR1MessageDecryptionContext
}
type RDecryptMessage struct {
	Content []byte
}

func DecryptMessage(ctx context.Context, p *PDecryptMessage) (*RDecryptMessage, wve.WVE) {
	wo := serdes.WaveWireObject{}
	rest, err := asn1.Unmarshal(p.Ciphertext, &wo.Content)
	if len(rest) != 0 || err != nil {
		return nil, wve.Err(wve.InvalidParameter, "message is malformed")
	}
	msg, ok := wo.Content.Content.(serdes.WaveEncryptedMessage)
	if !ok {
		return nil, wve.Err(wve.InvalidParameter, "ciphertext is not a wave encrypted message")
	}
	for _, k := range msg.Keys {
		directkey, ok := k.Content.(serdes.MessageKeyCurve25519ECDH)
		if ok {
			ddk, err := p.Decryptor.WR1DirectDecryptionKey(ctx)
			if err != nil {
				return nil, wve.Err(wve.InvalidParameter, "decrypting entity missing WR1 parameters")
			}
			contentsKey, err := ddk.DecryptMessage(ctx, directkey.Ciphertext)
			if err != nil {
				continue
			}
			if len(contentsKey) != 16+12 {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}
			content, ok := aesGCMDecrypt(contentsKey[:16], msg.Contents, contentsKey[16:])
			if !ok {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}
			return &RDecryptMessage{Content: content}, nil
		}

		wr1key, ok := k.Content.(serdes.MessageKeyWR1)
		if ok {
			if p.Dctx == nil {
				//We can't try decoding WR1 style messages
				continue
			}
			ns := HashSchemeInstanceFor(&wr1key.Namespace)
			var envelopeKey []byte
			//First get IBE key for namespace
			p.Dctx.WR1IBEKeysForPartitionLabel(ctx, ns, func(k EntitySecretKeySchemeInstance) bool {
				contents, err := k.DecryptMessage(ctx, wr1key.EnvelopeKeyIBEBN256)
				if err != nil {
					return true
				}
				envelopeKey = contents
				return false
			})
			if envelopeKey == nil {
				continue
			}
			if len(envelopeKey) != 16+12 {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}
			envelopeDER, ok := aesGCMDecrypt(envelopeKey[:16], wr1key.Envelope, envelopeKey[16:])
			if !ok {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}
			envelope := serdes.MessageKeyWR1Envelope{}
			rest, err := asn1.Unmarshal(envelopeDER, &envelope)
			if err != nil || len(rest) != 0 {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}

			//Now decrypt oaque
			var contentsKey []byte
			p.Dctx.WR1OAQUEKeysForContent(ctx, ns, envelope.Partition, func(k SlottedSecretKey) bool {
				var err error
				contentsKey, err = k.DecryptMessage(ctx, envelope.ContentsKey)
				if err == nil {
					return false
				}
				return true
			})
			if contentsKey == nil {
				continue
			}
			if len(contentsKey) != 16+12 {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}
			contents, ok := aesGCMDecrypt(contentsKey[:16], msg.Contents, contentsKey[16:])
			if !ok {
				return nil, wve.Err(wve.MalformedObject, "ciphertext is not correctly constructed")
			}
			return &RDecryptMessage{
				Content: contents,
			}, nil
		}
	}
	return nil, wve.Err(wve.MessageDecryptFailed, "could not decrypt message")
}
