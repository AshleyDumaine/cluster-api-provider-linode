# Etcd

This guide covers etcd configuration for the control plane of provisioned CAPL clusters.

## Default configuration

By default, etcd is configured to be on a separate device from the root filesystem on
control plane nodes. For each control plane machine, a volume backed by
[Linode Block Storage](https://www.linode.com/docs/products/storage/block-storage/)
is used. This volume is sized at 10 GB which is 
[the minimum size offered by the Linode Block Storage service](https://www.linode.com/docs/products/storage/block-storage/#technical-specifications).
