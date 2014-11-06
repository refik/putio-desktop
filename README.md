# Putio Desktop Client

This program periodically checks a folder in your put.io account. It creates the same folder, file structure in your computer. Downloads are done with multiple connections and this makes it fast.

## How to use

```
./putio-desktop -oauth-token=XXXXXX \
                -putio-folder="Send Home" \
                -local-path=/Users/refik/putio-files
```

### Options

- **-oauth-token** - You can get yours from [here](https://put.io/v2/oauth2/apptoken/1681).
- **-putio-folder** - The folder on put.io that will be watched for files. This folder has to be on the root of "Your Files".
- **-local-path** - The folder on your computer where the files on put.io will be downloaded to.



