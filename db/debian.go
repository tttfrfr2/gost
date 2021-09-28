package db

import (
	"errors"
	"fmt"

	"github.com/inconshreveable/log15"
	"github.com/spf13/viper"
	"github.com/vulsio/gost/models"
	"golang.org/x/xerrors"
	pb "gopkg.in/cheggaaa/pb.v1"
	"gorm.io/gorm"
)

// GetDebian :
func (r *RDBDriver) GetDebian(cveID string) (*models.DebianCVE, error) {
	c := models.DebianCVE{}
	if err := r.conn.Where(&models.DebianCVE{CveID: cveID}).First(&c).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		log15.Error("Failed to get Debian", "err", err)
		return nil, err
	}

	if err := r.conn.Model(&c).Association("Package").Find(&c.Package); err != nil {
		log15.Error("Failed to get Debian.Package", "err", err)
		return nil, err
	}

	newPkg := []models.DebianPackage{}
	for _, pkg := range c.Package {
		if err := r.conn.Model(&pkg).Association("Release").Find(&pkg.Release); err != nil {
			log15.Error("Failed to get Debian.Package.Release", "err", err)
			return nil, err
		}
		newPkg = append(newPkg, pkg)
	}
	c.Package = newPkg
	return &c, nil
}

// GetDebianMulti :
func (r *RDBDriver) GetDebianMulti(cveIDs []string) (map[string]models.DebianCVE, error) {
	m := map[string]models.DebianCVE{}
	for _, cveID := range cveIDs {
		cve, err := r.GetDebian(cveID)
		if err != nil {
			return nil, err
		}
		if cve != nil {
			m[cve.CveID] = *cve
		}
	}
	return m, nil
}

// InsertDebian :
func (r *RDBDriver) InsertDebian(cveJSON models.DebianJSON) (err error) {
	cves := ConvertDebian(cveJSON)
	if err = r.deleteAndInsertDebian(cves); err != nil {
		return fmt.Errorf("Failed to insert Debian CVE data. err: %s", err)
	}
	return nil
}
func (r *RDBDriver) deleteAndInsertDebian(cves []models.DebianCVE) (err error) {
	bar := pb.StartNew(len(cves))
	tx := r.conn.Begin()

	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		tx.Commit()
	}()

	// Delete all old records
	if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(models.DebianRelease{}).Error; err != nil {
		return fmt.Errorf("Failed to delete DebianRelease. err: %s", err)
	}
	if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(models.DebianPackage{}).Error; err != nil {
		return fmt.Errorf("Failed to delete DebianPackage. err: %s", err)
	}
	if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(models.DebianCVE{}).Error; err != nil {
		return fmt.Errorf("Failed to delete DebianCVE. err: %s", err)
	}

	batchSize := viper.GetInt("batch-size")
	if batchSize < 1 {
		return fmt.Errorf("Failed to set batch-size. err: batch-size option is not set properly")
	}

	for idx := range chunkSlice(len(cves), batchSize) {
		if err = tx.Create(cves[idx.From:idx.To]).Error; err != nil {
			return fmt.Errorf("Failed to insert. err: %s", err)
		}
		bar.Add(idx.To - idx.From)
	}
	bar.Finish()

	return nil
}

// ConvertDebian :
func ConvertDebian(cveJSONs models.DebianJSON) (cves []models.DebianCVE) {
	uniqCve := map[string]models.DebianCVE{}
	for pkgName, cveMap := range cveJSONs {
		for cveID, cve := range cveMap {
			var releases []models.DebianRelease
			for release, releaseInfo := range cve.Releases {
				r := models.DebianRelease{
					ProductName:  release,
					Status:       releaseInfo.Status,
					FixedVersion: releaseInfo.FixedVersion,
					Urgency:      releaseInfo.Urgency,
					Version:      releaseInfo.Repositories[release],
				}
				releases = append(releases, r)
			}

			pkg := models.DebianPackage{
				PackageName: pkgName,
				Release:     releases,
			}

			pkgs := []models.DebianPackage{pkg}
			if oldCve, ok := uniqCve[cveID]; ok {
				pkgs = append(pkgs, oldCve.Package...)
			}

			uniqCve[cveID] = models.DebianCVE{
				CveID:       cveID,
				Scope:       cve.Scope,
				Description: cve.Description,
				Package:     pkgs,
			}
		}
	}
	for _, c := range uniqCve {
		cves = append(cves, c)
	}
	return cves
}

var debVerCodename = map[string]string{
	"8":  "jessie",
	"9":  "stretch",
	"10": "buster",
	"11": "bullseye",
	"12": "bookworm",
	"13": "trixie",
}

// GetUnfixedCvesDebian gets the CVEs related to debian_release.status = 'open', major, pkgName.
func (r *RDBDriver) GetUnfixedCvesDebian(major, pkgName string) (map[string]models.DebianCVE, error) {
	return r.getCvesDebianWithFixStatus(major, pkgName, "open")
}

// GetFixedCvesDebian gets the CVEs related to debian_release.status = 'resolved', major, pkgName.
func (r *RDBDriver) GetFixedCvesDebian(major, pkgName string) (map[string]models.DebianCVE, error) {
	return r.getCvesDebianWithFixStatus(major, pkgName, "resolved")
}

func (r *RDBDriver) getCvesDebianWithFixStatus(major, pkgName, fixStatus string) (map[string]models.DebianCVE, error) {
	codeName, ok := debVerCodename[major]
	if !ok {
		log15.Error("Debian %s is not supported yet", "err", major)
		return nil, xerrors.Errorf("Failed to convert from major version to codename. err: Debian %s is not supported yet", major)
	}

	type Result struct {
		DebianCveID int64
	}

	results := []Result{}
	err := r.conn.
		Table("debian_packages").
		Select("debian_cve_id").
		Where("package_name = ?", pkgName).
		Scan(&results).Error

	if err != nil {
		if fixStatus == "open" {
			log15.Error("Failed to get unfixed cves of Debian", "err", err)
		} else {
			log15.Error("Failed to get fixed cves of Debian", "err", err)
		}
		return nil, err
	}

	m := map[string]models.DebianCVE{}
	for _, res := range results {
		debcve := models.DebianCVE{}
		if err := r.conn.
			Preload("Package.Release", "status = ? AND product_name = ?", fixStatus, codeName).
			Preload("Package", "package_name = ?", pkgName).
			Where(&models.DebianCVE{ID: res.DebianCveID}).
			First(&debcve).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, xerrors.Errorf("Failed to get DebianCVE. DB relationship may be broken, use `$ gost fetch debian` to recreate DB. err: %w", err)
			}
			log15.Error("Failed to get DebianCVE", res.DebianCveID, err)
			return nil, err
		}

		if len(debcve.Package) != 0 {
			for _, pkg := range debcve.Package {
				if len(pkg.Release) != 0 {
					m[debcve.CveID] = debcve
				}

			}
		}
	}

	return m, nil
}
