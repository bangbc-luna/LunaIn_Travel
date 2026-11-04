package listing

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bangbc-luna/lunain-travel/internal/listing/store"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

const (
	maxImages = 20

	imagePurpose = "listing_image"
	actionImages = "images"
)

// Image is one picture of a listing's gallery; SortOrder is its position.
type Image struct {
	FileID    uuid.UUID `json:"file_id"`
	SortOrder int       `json:"sort_order"`
}

// Images returns the listing's gallery in display order.
func (s *Service) Images(ctx context.Context, tx pgx.Tx, listingID uuid.UUID) ([]Image, error) {
	return listImages(ctx, tx, listingID)
}

func listImages(ctx context.Context, q store.DBTX, listingID uuid.UUID) ([]Image, error) {
	rows, err := store.New(q).ListListingImages(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("list listing images: %w", err)
	}
	out := make([]Image, len(rows))
	for i, r := range rows {
		out[i] = Image{FileID: r.FileID, SortOrder: int(r.SortOrder)}
	}
	return out, nil
}

func fileIDStrings(images []Image) []string {
	out := make([]string, len(images))
	for i, im := range images {
		out[i] = im.FileID.String()
	}
	return out
}

// SetImages replaces the whole gallery with fileIDs, in the order given
// (sort_order = position), and returns the listing and the new gallery. At
// most 20 distinct images; a file not already in the gallery must have been
// uploaded by actor with purpose listing_image (files already there, e.g.
// uploaded by another member, can be kept and re-ordered). coverFileID must be
// in the list; nil picks the first image, and an empty list clears the cover.
func (s *Service) SetImages(ctx context.Context, partnerID, actor, id uuid.UUID, fileIDs []uuid.UUID, coverFileID *uuid.UUID) (Listing, []Image, error) {
	if len(fileIDs) > maxImages {
		return Listing{}, nil, invalidListing(fmt.Sprintf("Tối đa %d ảnh cho mỗi Listing", maxImages))
	}
	seen := make(map[uuid.UUID]struct{}, len(fileIDs))
	for _, f := range fileIDs {
		if _, dup := seen[f]; dup {
			return Listing{}, nil, invalidListing("Danh sách ảnh có ảnh bị trùng")
		}
		seen[f] = struct{}{}
	}
	var cover *uuid.UUID
	switch {
	case coverFileID != nil:
		if _, ok := seen[*coverFileID]; !ok {
			return Listing{}, nil, invalidListing("Ảnh bìa phải nằm trong danh sách ảnh")
		}
		cover = coverFileID
	case len(fileIDs) > 0:
		cover = &fileIDs[0]
	}

	now := s.clock.Now()
	var out store.Listing
	var newImages []Image
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.requireActivePartner(ctx, tx, partnerID); err != nil {
			return err
		}
		cur, err := s.lockOwned(ctx, tx, partnerID, id)
		if err != nil {
			return err
		}
		old, err := listImages(ctx, tx, id)
		if err != nil {
			return err
		}
		inGallery := make(map[uuid.UUID]bool, len(old))
		for _, im := range old {
			inGallery[im.FileID] = true
		}
		for _, f := range fileIDs {
			if inGallery[f] {
				continue
			}
			if err := s.files.Owned(ctx, tx, f, actor, imagePurpose); err != nil {
				return err
			}
		}
		q := store.New(tx)
		if err := q.DeleteListingImages(ctx, id); err != nil {
			return fmt.Errorf("delete listing images: %w", err)
		}
		newImages = make([]Image, len(fileIDs))
		for i, f := range fileIDs {
			newImages[i] = Image{FileID: f, SortOrder: i}
			rowID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("listing image id: %w", err)
			}
			if err := q.InsertListingImage(ctx, store.InsertListingImageParams{
				ID: rowID, ListingID: id, FileID: f, SortOrder: int32(i), Now: now,
			}); err != nil {
				return fmt.Errorf("insert listing image: %w", err)
			}
		}
		out, err = q.SetListingCover(ctx, store.SetListingCoverParams{ID: id, PartnerID: partnerID, CoverFileID: cover, Now: now})
		if err != nil {
			return fmt.Errorf("set listing cover: %w", err)
		}
		return s.logChange(ctx, tx, id, actor, actionImages, map[string]change{
			"file_ids":      {fileIDStrings(old), fileIDStrings(newImages)},
			"cover_file_id": {val(cur.CoverFileID), val(cover)},
		}, now)
	})
	if err != nil {
		return Listing{}, nil, err
	}
	return fromRow(out), newImages, nil
}
