"""Credential namespace migration must never misroute a secret."""
import os
import unittest
from unittest.mock import patch
from ferry import resolve_plane_config
import sync_opencodex_fast as sync

class NamespaceTests(unittest.TestCase):
    def test_complete_new_and_old_pairs(self):
        for plane, url, token in [('management','MANAGEMENT_URL','MANAGEMENT_TOKEN'),('data','DATA_URL','API_KEY')]:
            for prefix in ('FERRY_','SMARTAPI_'):
                with patch.dict(os.environ,{prefix+url:'http://localhost:7900',prefix+token:'fixture-secret'},clear=True):
                    self.assertEqual(resolve_plane_config(plane),('http://localhost:7900','fixture-secret'))
                    self.assertEqual(resolve_plane_config(plane,'http://localhost:7901'),('http://localhost:7901','fixture-secret'))
    def test_no_cross_namespace_pairing_or_conflicts(self):
        for env in [
            {'FERRY_MANAGEMENT_URL':'http://localhost','SMARTAPI_MANAGEMENT_TOKEN':'fixture-secret'},
            {'FERRY_MANAGEMENT_TOKEN':'fixture-secret','SMARTAPI_MANAGEMENT_URL':'http://localhost'},
            {'FERRY_MANAGEMENT_URL':'http://localhost','FERRY_MANAGEMENT_TOKEN':'fixture-secret','SMARTAPI_MANAGEMENT_URL':'http://other','SMARTAPI_MANAGEMENT_TOKEN':'old-secret'},
            {'FERRY_MANAGEMENT_URL':'','SMARTAPI_MANAGEMENT_URL':'http://localhost','SMARTAPI_MANAGEMENT_TOKEN':'fixture-secret'},
        ]:
            with patch.dict(os.environ,env,clear=True),self.assertRaises(ValueError) as error:
                resolve_plane_config('management')
            self.assertNotIn('fixture-secret',str(error.exception))
            self.assertNotIn('old-secret',str(error.exception))
    def test_identical_aliases_and_unauthenticated_health(self):
        with patch.dict(os.environ,{p+k:v for p in ('FERRY_','SMARTAPI_') for k,v in [('DATA_URL','http://localhost'),('API_KEY','fixture-secret')]},clear=True):
            self.assertEqual(resolve_plane_config('data'),('http://localhost','fixture-secret'))
        with patch.dict(os.environ,{},clear=True):
            self.assertEqual(resolve_plane_config('data','http://localhost',False),('http://localhost',''))
    def test_sync_refuses_other_instance_before_fetch(self):
        provider={'adapter':'openai-chat','baseUrl':'https://other.example/v1'}
        with patch.dict(os.environ,{'FERRY_DATA_URL':'https://gateway.example','FERRY_API_KEY':'fixture-secret'},clear=True),patch.object(sync,'read_provider',return_value=provider),patch.object(sync,'fetch_models') as fetch:
            with self.assertRaises(sync.SyncError):sync.synchronize('ocx','existing-provider')
            fetch.assert_not_called()
    def test_explicit_existing_provider_keeps_its_identity(self):
        provider={'adapter':'openai-chat','baseUrl':'http://localhost/v1'}
        catalog={'data':[{'id':'test-model','supported_service_tiers':{'openai-chat':['priority'],'openai-responses':[]}}]}
        with patch.dict(os.environ,{'SMARTAPI_API_KEY':'fixture-secret'},clear=True),patch.object(sync,'read_provider',return_value=provider),patch.object(sync,'fetch_models',return_value=catalog),patch.object(sync,'ocx_command') as command:
            result=sync.synchronize('ocx','smart-api',apply=True)
            self.assertEqual(result['provider'],'smart-api')
            self.assertEqual(command.call_args_list[0].args[3],'providers.smart-api.modelSupportsServiceTier')

if __name__=='__main__':unittest.main()
